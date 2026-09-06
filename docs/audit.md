# The audit log

A tamper-evident record of what was done *to* the gateway, as opposed to what
was done *through* it.

Traffic is already described twice over — by the access log, by the spend ledger
and its reports, and by the console's traffic view. None of them answers the
question a compliance process actually asks: who minted this key, who blocked
that one, who was signed in to the console at 02:00 on a Sunday, and can you
show me that the answer has not been edited since.

## What is recorded

| Action | Recorded when |
|---|---|
| `key.generate` | a virtual key is minted, from `/key/generate` or the console |
| `key.update` | a key is edited — re-budgeted, re-scoped, blocked, unblocked |
| `key.delete` | a key is revoked |
| `console.sign_in` | an operator signs in to `/ui`, and when a sign-in is refused |
| `console.sign_out` | a console session is ended |
| `sso.grant` | an identity is issued a key by a login, and when one is refused |
| `sso.renew` | a key is reissued against the provider's refresh token, and when one is refused |
| `cache.purge` | the response cache is emptied |
| `gateway.start`, `gateway.stop` | this process starts and shuts down gracefully |

Inference is **not** audited. A record per request would put an fsync on the hot
path to describe traffic that
[observability.md](observability.md#recent-requests) and the spend ledger
already describe, and the audit log would become a second, worse access log.
That exclusion is what makes everything else here affordable.

## What a record looks like

One JSON object per line:

```json
{"seq":42,"at":"2026-03-11T09:14:02.118Z","prev":"9f3c…","action":"key.update",
 "actor":{"kind":"master_key"},"target_kind":"key","target":"a41b…",
 "outcome":"success","source_ip":"10.4.2.9","request_id":"req-6f2a91c0",
 "user_agent":"Mozilla/5.0 …","detail":{"alias":"finance","blocked":"true","via":"console"},
 "hash":"1d70…"}
```

| Field | Meaning |
|---|---|
| `seq` | monotonic within a chain, starting at 1 |
| `at` | UTC, RFC 3339 |
| `prev` | SHA-256 of the previous record; empty in the first |
| `action` | one of the actions above |
| `actor` | who acted — see below |
| `target_kind`, `target` | what was acted on: a key's **storage hash**, an identity's subject, a session's subject, the cache, the gateway |
| `outcome` | `success`, `refused` (the gateway declined) or `error` (it was allowed and then failed) |
| `reason` | why, on a refusal or an error |
| `source_ip` | the address the request arrived from |
| `request_id` | joins the record to the access log line for the same request |
| `user_agent` | the caller's, verbatim |
| `detail` | small, legible facts: the alias, the scope, the role, the device, and `via` — `api`, `console` or `sso` |
| `hash` | SHA-256 of this record with `hash` blanked |

`source_ip` is the gateway's own peer address, and `X-Forwarded-For` is
deliberately ignored. That header is attacker-controlled unless every hop in
front rewrites it, and the gateway cannot know whether they do; a record naming
an address the caller chose is worse than one naming the proxy, because it is
false and looks true. Behind a proxy, have the proxy log the client address.

### What is never recorded

No plaintext key, no master key, no OAuth or refresh token, no prompt or
completion body. A key appears as the storage hash it is addressed by
everywhere else — enough to join a record to `/key/list`, useless to whoever
walks off with the log. This is the same posture `internal/reqlog` takes and for
a sharper reason: the audit log is the artefact most likely to be copied into a
ticket.

## The actor

```json
{"kind":"master_key"}
{"kind":"sso_subject","id":"8f2c-…","display":"dev@example.com"}
{"kind":"unauthenticated"}
{"kind":"system"}
```

Today every administrative action is `master_key`, including everything done
from the console — because a console session **is** the master key, checked at
sign-in and carried in a cookie. There is nobody else to name, and inventing a
friendlier label would be recording a person the gateway never authenticated.

`sso_subject` already appears on `sso.grant` and `sso.renew`, where an identity
provider did authenticate someone. The day the console signs in through that
same provider, its records gain a real name with no change to this schema and no
migration of what is already written. That is the whole reason the actor is a
kind plus an id rather than one string.

## Tamper evidence

Each record carries the hash of the one before it, so:

- a field edited in place breaks that record's own hash;
- a line deleted from the middle breaks both the sequence and the next record's
  `prev`;
- a line replaced with a valid record from elsewhere breaks `prev`;
- removing the first records leaves a chain that does not start at 1.

```
$ gateway -verify-audit ./data/audit.jsonl
audit log ./data/audit.jsonl: verified
  records:   1284
  sequence:  1..1284
  last hash: 7c1e4a90…
```

and when it does not:

```
$ gateway -verify-audit ./data/audit.jsonl
audit log ./data/audit.jsonl: FAILED after 611 record(s)
gateway: ./data/audit.jsonl: audit chain broken at line 612 (seq 613): sequence
jumped from 611, so 1 record(s) were removed
```

The flag is read before the configuration file, so a chain someone hands you —
an archive, a copy pulled off a decommissioned host — can be checked without a
working gateway config.

### What it cannot do

A chain cannot detect the removal of its own tail. Delete the last ten lines and
what remains verifies perfectly, because the evidence that those lines existed
went with them. Closing that gap needs an anchor somewhere the gateway cannot
write: the last sequence number and hash, copied to a collector, a WORM bucket
or another host. `-verify-audit` prints both for exactly that purpose.

This is stated rather than papered over. The alternative — signing each record
with a key the gateway holds — moves the problem rather than solving it, since
whoever can rewrite the file can also use the key sitting beside it.

## Sinks

```yaml
audit:
  enabled: true
  sink: stdout          # stdout | file
  # sink: file
  # path: ./data/audit.jsonl
```

**stdout** is the default. It needs no path, creates no file, and introduces no
failure mode the process's own logging does not already have. Its limitation is
structural: a writer cannot read back what it wrote, so the chain restarts at
sequence 1 with every process. Within one process it is exactly as
tamper-evident as the file sink, which is enough to catch a record removed from
a shipped stream; continuity across restarts is whatever collects those streams.

**file** appends JSON Lines, `0600` in a `0700` directory, and fsyncs each
record. At startup it verifies the whole existing file and continues its chain.
Reading only the last line would be cheaper and would miss the point: a record
altered in the middle leaves a perfectly good last line. The read is bounded by
how many administrative actions the gateway has ever taken, which is a number in
the thousands.

A file whose chain does not verify **refuses to open**, so the gateway refuses to
start. Appending to it would produce one file that is half evidence and half
not, with nothing in it marking the boundary. Archive the file and let a new
chain begin — that is a decision for a person.

A database sink is the obvious next one and slots in behind the same interface;
nothing in `audit.Sink` assumes a file.

## Failure posture

**The write is synchronous, and a write that fails fails the action.** A key
that could not be audited is not minted. An audit trail that silently drops
records is worse than one that refuses the action: the first quietly becomes
untrue and nobody finds out until it is being read as evidence.

The record is written **before** the mutation is applied. If the audit write
fails, nothing happened and the caller gets a 500. If the mutation then fails, a
second record follows with `"outcome":"error"` and the same `request_id`, so a
reader sees the pair rather than an authorization with no consequence.

`gateway_audit_write_failures_total` counts the refusals. It is worth an alert:
from the outside they look like any other 500, and a non-zero value means the
gateway currently cannot be administered at all.

There are three deliberate exceptions.

**Signing out** is recorded but never refused. Refusing it would leave a live
console session behind, which is worse than an unrecorded sign-out.

**Unauthenticated refusals at `/key/*`** are not recorded. Those endpoints are
reachable by anyone who can open a socket, and an audit write fsyncs; recording
every attempt would hand a stranger a way to fill the operator's disk with
evidence of nothing. The access log already carries the 401 with its source and
request ID. A refused console sign-in *is* recorded, because it is a person at a
form and the one anybody asks about — see
[roadmap.md](roadmap.md#-sign-in-is-not-rate-limited) for the throttle that
endpoint still wants.

**SSO failures before an identity is established** — a failed token exchange, an
unverifiable ID token — are not recorded, for the same reason and because there
is no actor to name. A refusal *after* the provider vouched for someone is
recorded, since that is the interesting event: an authenticated identity denied
by this gateway's own rules.

## Reading it

```bash
# every key deleted last month
jq -r 'select(.action == "key.delete") | [.at, .actor.kind, .target] | @tsv' audit.jsonl

# everything one person did
jq -r 'select(.actor.id == "8f2c-…")' audit.jsonl

# refusals
jq -r 'select(.outcome != "success")' audit.jsonl
```

Joining to the access log is `request_id`. Joining to a key listing or a spend
report is `target` against the key's hash.

## What is still missing

The actor is `master_key` for every console action until the console signs in
through the identity provider. The record shape is ready for it; the sign-in is
not. See [roadmap.md](roadmap.md).
