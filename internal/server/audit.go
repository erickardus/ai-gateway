package server

import (
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/erickardus/ai-gateway/internal/audit"
	"github.com/erickardus/ai-gateway/internal/metrics"
	"github.com/erickardus/ai-gateway/internal/sso"
)

// UseAudit attaches the audit sink.
//
// Wired after construction like UseUI and UseSSO, and for the same reason: what
// the records are written to is a deployment decision, and the process
// assembling the gateway is the only thing that knows it. A nil sink leaves
// every administrative handler exactly as it was.
func (s *Server) UseAudit(sink audit.Sink) {
	if sink == nil {
		return
	}
	s.auditor = sink
}

// auditing reports whether records are being written, which is what the
// handlers branch on rather than reaching for the field.
func (s *Server) auditing() bool { return s.auditor != nil }

// adminEvent fills in what the gateway knows about the request an
// administrative action arrived on, leaving the action itself to the caller.
func (s *Server) adminEvent(r *http.Request, e audit.Event) audit.Event {
	e.Actor = s.adminActor(r)
	e.SourceIP = sourceIP(r)
	e.RequestID = RequestIDFrom(r.Context())
	e.UserAgent = r.UserAgent()
	if e.Outcome == "" {
		e.Outcome = audit.OutcomeSuccess
	}
	// Which surface the action came through, recorded because the two are the
	// same handler and otherwise indistinguishable afterwards: a key deleted
	// from a browser and one deleted by a script holding the master key are
	// different events to whoever is reading this a year later.
	if e.Detail == nil {
		e.Detail = make(map[string]string, 1)
	}
	e.Detail["via"] = surfaceOf(r)
	return e
}

// surfaceOf names the administrative surface a request arrived on.
func surfaceOf(r *http.Request) string {
	switch {
	case strings.HasPrefix(r.URL.Path, uiPrefix+"/"):
		return "console"
	case strings.HasPrefix(r.URL.Path, "/sso/"):
		return "sso"
	default:
		return "api"
	}
}

// ssoEvent describes something done on behalf of an identity the provider
// authenticated, which is the one case where the actor is a person rather than
// a credential.
//
// The subject is carried alongside the display name because a subject claim is
// an opaque identifier at most providers and an email at some. The opaque one
// is what survives a rename; the readable one is what makes the record legible
// without a second system to look it up in.
func (s *Server) ssoEvent(r *http.Request, action string, id *sso.Identity, e audit.Event) audit.Event {
	e.Action = action
	e = s.adminEvent(r, e)
	e.Actor = audit.SSOActor(id.Subject, id.Display())
	if e.Target == "" {
		// Nothing was issued, so the identity is what the action was about.
		e.TargetKind = audit.TargetIdentity
		e.Target = id.Subject
		return e
	}
	e.TargetKind = audit.TargetKey
	return e
}

// adminActor names who is acting on an administrative endpoint.
//
// The console and the master-key endpoints reach the same handlers, so the
// actor is decided from the request rather than from which route it arrived on.
// A valid console session is asked first: it is the only place a subject other
// than the master key can currently come from, and it is where an SSO-signed-in
// operator will appear the day the console accepts one.
//
// Today that subject is always "master", so this reports the master key and
// says nothing about a person. That is the honest record: every console session
// *is* the master key, and a friendlier label would be naming someone the
// gateway never authenticated.
func (s *Server) adminActor(r *http.Request) audit.Actor {
	subject, ok := s.uiSessionSubject(r)
	if !ok || subject == uiSubjectMaster {
		return audit.MasterKeyActor()
	}
	return audit.SSOActor(subject, "")
}

// sourceIP is the address the request arrived from.
//
// X-Forwarded-For is deliberately not read. It is attacker-controlled unless
// every hop in front of the gateway rewrites it, the gateway has no way to know
// whether they do, and an audit record naming an address the caller chose is
// worse than one naming the proxy: the first is false and looks true. The
// gateway's own peer address is a fact. An operator who needs the client
// address behind a proxy should have the proxy log it.
func sourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// recordAudit seals one event and reports whether the caller may proceed.
//
// A failed write refuses the request with a 500 and the mutation never happens.
// See the audit package comment for why an unrecorded administrative action is
// treated as worse than a refused one, and why this is affordable: it is off
// the inference path entirely.
//
// It is called *before* the action it describes is applied, so that a refusal
// here leaves nothing behind. recordAuditFailure is the other half of that
// arrangement.
func (s *Server) recordAudit(w http.ResponseWriter, r *http.Request, e audit.Event) bool {
	err := s.recordAuditErr(r, e)
	if err == nil {
		return true
	}
	// A sealed chain says something different to the operator holding the
	// master key than a failed write does. One is "try again"; the other is
	// "nobody can administer this gateway until the chain is dealt with", and
	// an operator who reads the first when it is the second will retry for a
	// while before going to look at the logs.
	var sealed *audit.SealedError
	if errors.As(err, &sealed) {
		writeError(w, http.StatusInternalServerError, "internal_error",
			"the action was refused because this gateway's audit chain no longer verifies and has been sealed; no administrative action will be accepted until it is archived — see the gateway's logs")
		return false
	}
	writeError(w, http.StatusInternalServerError, "internal_error",
		"the action was refused because it could not be written to the audit log")
	return false
}

// recordAuditErr is recordAudit for the callers that render their own failure —
// the SSO flows, which answer a browser redirect or an OAuth-shaped error
// rather than the gateway's JSON envelope.
func (s *Server) recordAuditErr(r *http.Request, e audit.Event) error {
	if !s.auditing() {
		return nil
	}
	if _, err := s.auditor.Record(r.Context(), e); err != nil {
		s.auditWriteFailed(r, e, err)
		return err
	}
	return nil
}

// recordAuditQuietly writes a record for an action that must proceed whether or
// not it is recorded, logging and counting a failure instead of returning it.
//
// It is the exception to the gateway's audit posture, and there are only two of
// them: signing out, where refusing would leave a live session behind, and the
// follow-up record for an action that has already failed, where there is
// nothing left to refuse.
func (s *Server) recordAuditQuietly(r *http.Request, e audit.Event) {
	if !s.auditing() {
		return
	}
	if _, err := s.auditor.Record(r.Context(), e); err != nil {
		s.auditWriteFailed(r, e, err)
	}
}

// recordAuditFailure follows a recorded action that then failed to take effect.
//
// The first record said the gateway had authorized the action; this one says it
// did not happen. Both carry the same request ID, so a reader sees the pair
// rather than an authorization with no consequence. It is best effort by
// necessity — the action has already failed, and there is nothing left to
// refuse — so a write failure here is logged and counted rather than returned.
func (s *Server) recordAuditFailure(r *http.Request, e audit.Event, cause error) {
	if !s.auditing() {
		return
	}
	e.Outcome = audit.OutcomeError
	e.Reason = cause.Error()
	s.recordAuditQuietly(r, e)
}

// detail builds an event's detail map, dropping any pair with nothing to say.
//
// An empty value is left out rather than written as "": a record that says the
// scope is the empty string invites a reader to wonder which scope that was,
// where an absent field plainly says the key has none.
func detail(kv ...string) map[string]string {
	out := make(map[string]string, len(kv)/2+1)
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			out[kv[i]] = kv[i+1]
		}
	}
	return out
}

// auditWriteFailed reports a sink that would not take a record.
//
// It goes to the ordinary log and to a counter because it is the one failure
// here nobody sees from the outside: the caller gets a 500 that looks like any
// other, and only this line says the gateway is currently unable to administer
// anything.
func (s *Server) auditWriteFailed(r *http.Request, e audit.Event, err error) {
	msg := "an administrative action could not be written to the audit log"
	var sealed *audit.SealedError
	if errors.As(err, &sealed) {
		msg = "an administrative action was refused because the audit chain is sealed; it will take no records until somebody archives it"
	}
	s.log.Error(msg, "action", e.Action, "error", err, "request_id", RequestIDFrom(r.Context()))
	s.metrics.Add(metrics.MAuditFailures, 1, "action", e.Action)
}
