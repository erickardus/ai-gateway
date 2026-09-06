package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/erickardus/ai-gateway/internal/audit"
	"github.com/erickardus/ai-gateway/internal/config"
)

// newAuditSink builds the configured audit sink, or nil when auditing is off.
//
// Nil rather than a sink that discards: a discarding sink would sit behind the
// same interface as a real one and answer every write with success, which is
// the one lie this package must not be able to tell. The server checks for the
// absence instead.
func newAuditSink(ctx context.Context, cfg config.AuditConfig, log *slog.Logger) (audit.Sink, error) {
	if !cfg.On() {
		log.Warn("administrative actions are not being audited; set audit.enabled to record them")
		return nil, nil
	}
	switch cfg.SinkKind() {
	case config.AuditSinkPostgres:
		sink, err := audit.OpenPostgres(ctx, audit.PostgresOptions{
			DSN:      cfg.DSN,
			Timeout:  cfg.Timeout,
			MaxConns: cfg.MaxConns,
			Instance: cfg.Instance,
		}, log)
		if err != nil {
			return nil, err
		}
		reportSealed(log, sink.Sealed(), audit.PostgresTarget(cfg.DSN))
		return sink, nil
	case config.AuditSinkFile:
		sink, err := audit.OpenFile(cfg.Path)
		if err != nil {
			return nil, err
		}
		if reason := sink.Sealed(); reason == "" {
			log.Info("audit log opened", "sink", config.AuditSinkFile, "path", sink.Path())
		} else {
			reportSealed(log, reason, sink.Path())
		}
		return sink, nil
	default:
		// Said out loud, because the difference matters and is invisible from
		// the outside: stdout records are as tamper-evident as the file sink's
		// within one process and start a new chain at every restart, so
		// continuity across restarts is whatever collects them.
		log.Info("audit log writing to stdout; the hash chain restarts with each process",
			"sink", config.AuditSinkStdout)
		return audit.NewStdoutSink(), nil
	}
}

// reportSealed says, at the top of the log, that this gateway will refuse every
// administrative action until somebody deals with its audit chain.
//
// Said three ways because a sealed sink is otherwise invisible: nothing about
// it shows up until an operator tries to mint a key and gets a 500 that looks
// like any other. This line is the first; gateway_audit_sealed is the second,
// registered in telemetry.go; the refusal itself is the third.
func reportSealed(log *slog.Logger, reason, where string) {
	if reason == "" {
		return
	}
	log.Error("the audit chain does not verify, so this gateway will refuse every administrative action until it is dealt with",
		"where", where, "reason", reason,
		"remedy", "run `gateway -verify-audit` to see where the chain breaks, archive it, and let a new chain begin")
}

// recordGatewayStart writes the first record of this process's run.
//
// A sealed chain is not a reason to refuse to start — that is the whole point
// of sealing, and reportSealed has already said so — but every other failure
// is, which is the same posture the administrative handlers take and for the
// same reason: a gateway that cannot write to its audit log cannot be
// administered, so starting one would only defer the failure to whoever next
// tries to mint a key.
func recordGatewayStart(ctx context.Context, sink audit.Sink, configPath string, cfg *config.Config) error {
	_, err := sink.Record(ctx, audit.Event{
		Action:     audit.ActionGatewayStart,
		Actor:      audit.SystemActor(),
		TargetKind: audit.TargetGateway,
		Target:     version,
		Outcome:    audit.OutcomeSuccess,
		Detail: map[string]string{
			"config_path":   configPath,
			"config_digest": configDigest(configPath),
			"addr":          cfg.Server.Addr,
		},
	})
	var sealed *audit.SealedError
	if errors.As(err, &sealed) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("record the gateway start in the audit log: %w", err)
	}
	return nil
}

// configDigest fingerprints the configuration this process started with.
//
// The digest is recorded rather than the configuration, which holds every
// upstream credential and the master key. It answers the question an auditor
// actually asks — was this the config that was reviewed, and did it change
// between these two restarts — without the log becoming a second place the
// gateway's secrets live.
//
// A file that cannot be read yields an empty digest rather than an error: by
// this point the configuration has already been loaded and validated from it,
// so failing here would refuse to start over a fact nobody depends on.
func configDigest(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	// Sixteen hex characters is enough to tell two configurations apart on
	// sight and short enough to read out. Nothing verifies against it.
	return hex.EncodeToString(sum[:])[:16]
}

// verifyAuditLog walks a chain and reports what it found, for -verify-audit.
//
// It prints the last sequence number and hash on success because those two are
// the anchor: a chain cannot detect the removal of its own tail, so an operator
// who records them somewhere the gateway cannot write can detect exactly that
// later. The audit package comment explains why that gap exists.
func verifyAuditLog(target string) error {
	if isPostgresDSN(target) {
		return verifyAuditDB(target)
	}
	summary, err := audit.VerifyFile(target)
	if err != nil {
		// A broken chain reports how much of it did verify before the error
		// itself, because that is the first thing anyone asks. A file that
		// could not be opened at all has no such answer and gets only the
		// error.
		var broken *audit.BreakError
		if errors.As(err, &broken) {
			fmt.Printf("audit log %s: FAILED after %d record(s)\n", target, summary.Records)
		}
		return err
	}
	reportChain(target, summary)
	return nil
}

// verifyAuditDB verifies a shared chain. It is the same report, over the same
// verifier, against rows instead of lines — and it names the database without
// its password, since this output is the sort of thing that ends up pasted into
// a ticket.
func verifyAuditDB(dsn string) error {
	ctx, cancel := context.WithTimeout(context.Background(), verifyTimeout)
	defer cancel()

	where := audit.PostgresTarget(dsn)
	summary, err := audit.VerifyPostgres(ctx, dsn)
	if err != nil {
		var broken *audit.BreakError
		if errors.As(err, &broken) {
			fmt.Printf("audit log %s: FAILED after %d record(s)\n", where, summary.Records)
		}
		return fmt.Errorf("%s: %w", where, err)
	}
	reportChain(where, summary)
	return nil
}

// reportChain prints what verified, and the two numbers an operator is supposed
// to take away and keep.
func reportChain(where string, summary audit.Summary) {
	if summary.Records == 0 {
		fmt.Printf("audit log %s: empty; no administrative actions have been recorded\n", where)
		return
	}
	fmt.Printf("audit log %s: verified\n", where)
	fmt.Printf("  records:   %d\n", summary.Records)
	fmt.Printf("  sequence:  %d..%d\n", summary.FirstSeq, summary.LastSeq)
	fmt.Printf("  last hash: %s\n", summary.LastHash)
	fmt.Println("  Keep the last sequence and hash somewhere this gateway cannot write.")
	fmt.Println("  A hash chain cannot detect the removal of its own tail; that anchor can.")
}

// verifyTimeout bounds the whole walk rather than one query, because that is
// what the operator is waiting on. Generous: a chain with a year of a large
// fleet's administration in it is hundreds of thousands of rows.
const verifyTimeout = 10 * time.Minute

// isPostgresDSN decides whether -verify-audit was handed a database or a file.
//
// The two URL schemes are unambiguous. The key/value form is recognised by the
// two words that never begin a path, which is what an operator who copied the
// dsn out of their configuration will paste; anything else is treated as a
// path, and a path that does not exist reports itself plainly.
func isPostgresDSN(s string) bool {
	return strings.HasPrefix(s, "postgres://") ||
		strings.HasPrefix(s, "postgresql://") ||
		strings.Contains(s, "host=") ||
		strings.Contains(s, "dbname=")
}
