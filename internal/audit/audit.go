// Package audit records the gateway's administrative actions as a
// tamper-evident chain: who did what to which key, from where, and whether it
// worked.
//
// It exists because the access log and the spend ledger both answer questions
// about traffic, and an auditor asks about administration. Minting a key,
// blocking one, deleting one and signing in to the console are the actions that
// change what the gateway will do for everyone else, and they are exactly the
// ones an ordinary log line records as an afterthought — "virtual key deleted",
// with no statement of who was holding the credential when it happened.
//
// # What it holds, and what it refuses to
//
// The same posture as reqlog: metadata, never content. No plaintext key, no
// master key, no OAuth or refresh token, no prompt or completion body. A key is
// named by the storage hash it is addressed by everywhere else, which is enough
// to join a record to a key listing and useless to whoever steals the log. That
// restraint is not decoration — an audit log is read by more people than any
// other artefact the gateway produces, and it is the one file a compliance
// process will happily copy into a ticket.
//
// # Tamper evidence
//
// Every record carries a monotonic sequence number and the SHA-256 of the
// record before it, so a line that is altered or removed from the middle breaks
// the chain at that point and Verify says where. The hash covers the record's
// own JSON with the hash field blanked, which is why the encoding has to be
// reproducible rather than merely parseable.
//
// What a chain cannot do on its own is detect truncation of its own tail: an
// attacker who deletes the last ten lines leaves a shorter chain that verifies
// perfectly. Closing that needs an anchor kept somewhere the gateway cannot
// write — a copy of the last hash and sequence number in a collector, a WORM
// bucket, another host. Both are printed by Verify for exactly that purpose.
// Saying so here is better than implying a property the file does not have.
//
// # Failure posture
//
// A write is synchronous, and a write that fails fails the action it was
// recording. An audit trail that silently drops records is worse than one that
// refuses the action: the first quietly becomes untrue, and nobody finds out
// until it is being read as evidence.
//
// That is affordable only because of what is *not* audited. Inference requests
// are not — they are reqlog's and the spend ledger's business — so nothing here
// sits on the hot path. Administrative mutations are rare, measured in a
// handful a day on a busy gateway, so a record can be fsynced before the action
// it describes is allowed to proceed.
//
// The ordering is deliberately write-ahead: the record is sealed and durable
// *before* the mutation is applied. If the audit write fails, nothing happened
// and the caller is told so. If the mutation then fails, a second record with
// an "error" outcome follows, carrying the same request ID, and a reader sees
// the pair. The alternative — record after the fact — cannot fail the mutation
// it is describing, because by then it has already happened; and the other
// alternative — undo the mutation when the audit write fails — needs a
// transaction spanning the key store and this package, which is a great deal of
// machinery for a case that means the disk is full.
//
// # Sinks
//
// A Sink takes a context and returns an error, and holds no assumptions about
// files. The two shipped here write JSON Lines to a file and to stdout; a
// database sink slots in behind the same three methods. The file sink is the
// one that continues a chain across restarts, because it is the only one that
// can read back what it wrote.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Actions the gateway records. They are dotted rather than free text so that a
// query over a year of records can group on the string without a taxonomy
// living only in whoever wrote the query's head.
const (
	ActionKeyGenerate = "key.generate"
	ActionKeyUpdate   = "key.update"
	ActionKeyDelete   = "key.delete"

	ActionConsoleSignIn  = "console.sign_in"
	ActionConsoleSignOut = "console.sign_out"

	// ActionSSOGrant and ActionSSORenew are the two ways an identity ends up
	// holding a virtual key. They are separate actions because a renewal is
	// evidence the identity is still valid at the provider, which a grant made
	// months ago is not.
	ActionSSOGrant = "sso.grant"
	ActionSSORenew = "sso.renew"

	ActionCachePurge = "cache.purge"

	ActionGatewayStart = "gateway.start"
	ActionGatewayStop  = "gateway.stop"
)

// Outcomes.
//
// A refusal and an error are kept apart because they mean opposite things about
// the gateway: refused is the gateway working — a wrong master key, an identity
// in no granted group — and error is the gateway failing after it had already
// decided to allow the action.
const (
	OutcomeSuccess = "success"
	OutcomeRefused = "refused"
	OutcomeError   = "error"
)

// Target kinds, so a target string is never ambiguous about what it names.
const (
	TargetKey      = "key"
	TargetIdentity = "identity"
	TargetSession  = "session"
	TargetCache    = "cache"
	TargetGateway  = "gateway"
)

// ActorKind is how the gateway knew who was acting.
type ActorKind string

const (
	// ActorMasterKey is whoever presented the master key — at the management
	// endpoints directly, or at the console's sign-in, whose session is the
	// master key wearing a cookie.
	//
	// It names a credential rather than a person, and deliberately so: today
	// there is no person to name. Every console session is the master key, so
	// recording "master" is the whole truth available, and inventing a nicer
	// label would be recording a person the gateway never authenticated.
	ActorMasterKey ActorKind = "master_key"
	// ActorSSOSubject is an identity the configured provider authenticated,
	// identified by its subject claim. This is the kind the console will start
	// producing the day it signs in through the identity provider, at which
	// point these records gain a real name with no change to their shape.
	ActorSSOSubject ActorKind = "sso_subject"
	// ActorUnauthenticated is a caller who presented nothing the gateway
	// accepted. It is not an absence: a refused sign-in has an actor, and it is
	// this one.
	ActorUnauthenticated ActorKind = "unauthenticated"
	// ActorSystem is the process itself, for the events no caller asked for.
	ActorSystem ActorKind = "system"
)

// Actor is who performed an action.
//
// Kind and ID are separate fields rather than one qualified string because they
// are answers to different questions — how the gateway knew, and who it was —
// and because the day an SSO subject appears here, everything already written
// stays readable without a migration.
type Actor struct {
	Kind ActorKind `json:"kind"`
	// ID identifies the actor within its kind: the provider's subject claim for
	// an SSO identity, and empty for the master key, which has no identity to
	// carry. It is never a credential.
	ID string `json:"id,omitempty"`
	// Display is a human-readable label — an email or a name from the identity
	// provider — kept beside the subject because a subject claim is a UUID in
	// most providers and unreadable in a report.
	Display string `json:"display,omitempty"`
}

// MasterKeyActor is the actor for anything authenticated by the master key.
func MasterKeyActor() Actor { return Actor{Kind: ActorMasterKey} }

// SSOActor names an identity the provider authenticated.
func SSOActor(subject, display string) Actor {
	return Actor{Kind: ActorSSOSubject, ID: subject, Display: display}
}

// Unauthenticated is the actor for a request that presented no credential the
// gateway accepted.
func Unauthenticated() Actor { return Actor{Kind: ActorUnauthenticated} }

// SystemActor is the process acting on its own behalf, at startup and shutdown.
func SystemActor() Actor { return Actor{Kind: ActorSystem} }

// Event is one administrative action, as its caller describes it. A Sink seals
// it into a Record.
type Event struct {
	Action string
	Actor  Actor
	// TargetKind and Target name what was acted on. For a key that is its
	// storage hash — never the key itself, which exists only in the response
	// that issued it.
	TargetKind string
	Target     string
	Outcome    string
	// Reason explains a refusal or an error in the words the operator would be
	// told. It carries no credential, because the refusals it describes are
	// about credentials.
	Reason string

	SourceIP  string
	RequestID string
	UserAgent string

	// Detail carries the small, operator-legible facts that make a record
	// answerable without a second lookup — an alias, a scope, whether a key was
	// blocked. It is a map rather than more fields because what is worth saying
	// differs per action, and a struct with a column per action would be mostly
	// empty on every row.
	//
	// It must never hold credential material or request content. Nothing here
	// enforces that; the callers in internal/server do, and a test holds them
	// to it.
	Detail map[string]string
}

// Record is a sealed Event: the event, its place in the chain, and the hash
// that binds it there.
//
// The field order is the JSON field order, and the JSON is what is hashed, so
// reordering these fields invalidates every chain ever written. That is why
// there is no omitempty on seq, at, prev, action, actor, outcome or hash: a
// field that sometimes disappears is a field whose absence has to be reproduced
// exactly at verification time.
type Record struct {
	Seq uint64    `json:"seq"`
	At  time.Time `json:"at"`
	// Prev is the previous record's hash, hex encoded. It is empty in the first
	// record of a chain, which is how a verifier tells "this is the beginning"
	// from "the beginning was deleted".
	Prev string `json:"prev"`

	Action     string `json:"action"`
	Actor      Actor  `json:"actor"`
	TargetKind string `json:"target_kind,omitempty"`
	Target     string `json:"target,omitempty"`
	Outcome    string `json:"outcome"`
	Reason     string `json:"reason,omitempty"`

	SourceIP  string            `json:"source_ip,omitempty"`
	RequestID string            `json:"request_id,omitempty"`
	UserAgent string            `json:"user_agent,omitempty"`
	Detail    map[string]string `json:"detail,omitempty"`

	// Hash is the SHA-256 of this record's own JSON with this field set to the
	// empty string. It is last so that a line read by a person ends with the
	// seal.
	Hash string `json:"hash"`
}

// Sink writes sealed records somewhere durable.
//
// Record returns the sealed record so a caller can report its sequence number,
// and returns an error the caller is expected to treat as fatal to whatever it
// was about to do. Both take a context because the sink that is coming — a
// database — will need one, and adding it later would change every call site
// for no behavioural reason.
type Sink interface {
	Record(ctx context.Context, e Event) (Record, error)
	// Close releases the sink. It is safe to call on a sink that never wrote
	// anything, and a Sink that was closed refuses further records rather than
	// dropping them.
	Close() error
}

// chain holds a sink's position in its hash chain and serializes writes.
//
// The emit callback runs while the lock is held, which is what keeps the order
// of the bytes on disk identical to the order of the sequence numbers. Sealing
// under one lock and writing under another would produce a file whose lines are
// individually valid and collectively out of order, which verification would
// then report as tampering.
type chain struct {
	mu     sync.Mutex
	seq    uint64
	prev   string
	closed bool
	now    func() time.Time
	emit   func(line []byte) error
}

// errClosed is returned rather than dropping a record, because a dropped record
// is the failure this package exists to make impossible.
var errClosed = fmt.Errorf("audit sink is closed")

// maxFieldBytes bounds each free-text field a record carries.
//
// Several of them arrive from outside and have no length the gateway chose: a
// User-Agent header, a reason that may quote what a caller sent, a display name
// minted by an identity provider. A record is sealed whatever its size, but
// Verify reads the file back with a bounded scanner, so an oversized line
// written today is a chain that cannot be read tomorrow — and OpenFile refuses
// to start on a chain it cannot verify. Without this, a caller who can reach any
// audited endpoint can choose a header that stops the gateway from ever booting
// again.
//
// Clipping happens before the hash is taken, so a clipped record verifies
// exactly as any other does.
const maxFieldBytes = 1024

// clip shortens s to at most n bytes, marking that it did.
//
// The result is valid UTF-8 even when the cut lands inside a rune, so a record
// reads the same way in a terminal as it does through jq — and so that two
// readers computing the hash of the same bytes cannot disagree about what those
// bytes say.
func clip(s string, n int) string {
	if len(s) <= n {
		return strings.ToValidUTF8(s, "")
	}
	const ellipsis = "\u2026"
	return strings.ToValidUTF8(s[:n-len(ellipsis)], "") + ellipsis
}

// clipDetail bounds every value in a detail map. The keys are the gateway's
// own and need no bounding; the values are not always.
func clipDetail(d map[string]string) map[string]string {
	if d == nil {
		return nil
	}
	out := make(map[string]string, len(d))
	for k, v := range d {
		out[k] = clip(v, maxFieldBytes)
	}
	return out
}

// clipActor bounds the two actor fields an identity provider supplies. A
// subject claim and a display name are as much outside input as a header is;
// they simply arrive vouched for.
func clipActor(a Actor) Actor {
	a.ID = clip(a.ID, maxFieldBytes)
	a.Display = clip(a.Display, maxFieldBytes)
	return a
}

func (c *chain) record(e Event) (Record, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Record{}, errClosed
	}

	rec := Record{
		Seq:        c.seq + 1,
		At:         c.now().UTC(),
		Prev:       c.prev,
		Action:     e.Action,
		Actor:      clipActor(e.Actor),
		TargetKind: e.TargetKind,
		Target:     clip(e.Target, maxFieldBytes),
		Outcome:    e.Outcome,
		Reason:     clip(e.Reason, maxFieldBytes),
		SourceIP:   e.SourceIP,
		RequestID:  e.RequestID,
		UserAgent:  clip(e.UserAgent, maxFieldBytes),
		Detail:     clipDetail(e.Detail),
	}
	line, err := seal(&rec)
	if err != nil {
		return Record{}, err
	}
	if err := c.emit(line); err != nil {
		return Record{}, err
	}
	c.seq, c.prev = rec.Seq, rec.Hash
	return rec, nil
}

// resume points the chain at the tail of an existing one.
func (c *chain) resume(seq uint64, hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq, c.prev = seq, hash
}

func (c *chain) close(closer func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if closer == nil {
		return nil
	}
	return closer()
}

// seal computes a record's hash and renders the line that carries it.
//
// The hash is taken over the record with Hash blank rather than over some
// separate canonical form, so that verification is the same operation as
// sealing: decode the line, blank the field, re-encode, compare. A second
// canonicalization would be a second thing to get subtly wrong, and the
// symptom would be a chain that fails to verify itself.
func seal(r *Record) ([]byte, error) {
	r.Hash = ""
	body, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("encode audit record: %w", err)
	}
	sum := sha256.Sum256(body)
	r.Hash = hex.EncodeToString(sum[:])

	line, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("encode audit record: %w", err)
	}
	return append(line, '\n'), nil
}

// hashOf recomputes a record's seal, for verification.
func hashOf(r Record) (string, error) {
	r.Hash = ""
	body, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("encode audit record: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}
