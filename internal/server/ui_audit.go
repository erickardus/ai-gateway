package server

import (
	"net/http"
	"strconv"

	"github.com/erickardus/ai-gateway/internal/audit"
)

// Bounds on how much of the chain one page may ask for. A record is a few
// hundred bytes and the console renders a table of them; five hundred is
// already more than anybody reads, and the chain is not something to page a
// year of through a browser — that is what `gateway -verify-audit` and the
// database are for.
const (
	auditDefaultLimit = 100
	auditMaxLimit     = 500
)

// Notes the audit view carries, which are the difference between a feature that
// looks broken and one that is switched off.
const (
	auditUnreadableNote = "This sink writes records it cannot read back, so the console cannot show them. Only `audit.sink: file` and `audit.sink: postgres` keep a chain the gateway can re-read; the stdout sink hands its records to whatever collects this process's logs, and they are visible there. Set audit.sink to file or postgres to make the log visible here."
	auditDisabledNote   = "No audit sink is attached, so administrative actions are not being recorded at all. Set `audit.enabled: true` with `audit.sink: file` or `postgres` to keep a chain and to see it here."
	auditFileNote       = "Newest first, from this host's own chain. A file chain is per host, so a fleet keeps one of these per replica and no ordering exists between them; `audit.sink: postgres` is the arrangement that produces a single chain for a fleet. The chain was re-verified for this request, which is affordable because a file holds only one host's administrative history."
	auditPostgresNote   = "Newest first, from the chain every instance of this gateway shares. The chain was not verified for this request: verification walks the whole table, which is the fleet's entire administrative history, and a page view is the wrong thing to pay that for. Run `gateway -verify-audit <dsn>`, or a scheduled job, to verify it."
	auditReadableNote   = "Newest first. This sink was not verified for this request."
)

// handleUIAudit serves the tail of the audit chain.
//
// The console could not see the audit log at all before this, which made the
// one artefact an auditor asks for the one thing only somebody with shell
// access could read. It is a read path and nothing more: records are appended
// by the actions they describe, and an endpoint that could write one would be
// an endpoint that can forge the evidence.
//
// A sink that cannot be read back answers 200 with readable false rather than
// an error. The stdout sink is the default, so an error here would mean the
// console reporting a failure on a gateway that is configured exactly as
// shipped — and a feature that is off is not a failure.
func (s *Server) handleUIAudit(w http.ResponseWriter, r *http.Request) {
	limit := auditDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = min(n, auditMaxLimit)
		}
	}

	body := map[string]any{
		"sink": s.auditSinkKind(),
		// Always an array, so the console renders an empty table rather than
		// branching on a null it would otherwise have to expect here and
		// nowhere else.
		"records":     []audit.Record{},
		"count":       0,
		"sealed":      false,
		"seal_reason": "",
	}
	// Sealing is reported whether or not the chain can be read, because it is
	// the state that matters most and the one nothing else surfaces: a sealed
	// gateway refuses every administrative action, and until somebody asks it
	// to do one there is no other sign.
	if sealer, ok := s.auditor.(interface{ Sealed() string }); ok {
		if reason := sealer.Sealed(); reason != "" {
			body["sealed"], body["seal_reason"] = true, reason
		}
	}

	reader, ok := s.auditor.(audit.Reader)
	if !ok {
		body["readable"] = false
		body["note"] = auditUnreadableNote
		if s.auditor == nil {
			body["note"] = auditDisabledNote
		}
		writeJSON(w, http.StatusOK, body)
		return
	}

	records, err := reader.Tail(r.Context(), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	body["readable"] = true
	body["records"] = records
	body["count"] = len(records)
	body["note"] = auditReadableNote

	// Verification is run for the file sink and for no other, and the
	// difference is cost rather than capability. Walking a file is bounded by
	// what one host has ever administered — the same read OpenFile makes at
	// every start — while walking the Postgres chain is a full scan of the
	// fleet's whole history across a network, which is what `gateway
	// -verify-audit` and a nightly job are for. Where it was not checked the
	// verification fields are absent rather than false: a false with no reason
	// beside it reads as "this chain is broken", which would be the console
	// inventing an alarm out of a cost decision.
	switch sink := s.auditor.(type) {
	case *audit.FileSink:
		body["note"] = auditFileNote
		summary, err := sink.Verify(r.Context())
		body["verified"] = err == nil
		body["verified_through"] = summary.LastSeq
		if err != nil {
			body["verify_error"] = err.Error()
		}
	case *audit.PostgresSink:
		body["note"] = auditPostgresNote
	}

	writeJSON(w, http.StatusOK, body)
}

// auditSinkKind names the sink in the operator's own words.
//
// It reads the configuration rather than the sink's Go type because the config
// is what an operator would change to alter this answer, and because "stdout"
// is a claim about where a WriterSink points that the type itself cannot make.
// The process that assembles the gateway builds the sink from exactly this
// field, so the two cannot disagree.
func (s *Server) auditSinkKind() string {
	if s.auditor == nil {
		return "none"
	}
	return s.cfg.Audit.SinkKind()
}
