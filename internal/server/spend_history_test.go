package server

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/spend"
)

// recordingHistory answers with what it was configured to, and remembers the
// query it was asked, which is what these tests are actually about: the
// translation from a URL an operator typed into a query the store can answer.
type recordingHistory struct {
	asked   spend.Query
	buckets []spend.Bucket
	rows    []spend.Row
	err     error
}

func (h *recordingHistory) Append(context.Context, spend.Entry) error { return nil }

func (h *recordingHistory) Series(_ context.Context, q spend.Query) ([]spend.Bucket, error) {
	h.asked = q
	return h.buckets, h.err
}

func (h *recordingHistory) EachRow(_ context.Context, q spend.Query, fn func(spend.Row) error) error {
	h.asked = q
	for _, r := range h.rows {
		if err := fn(r); err != nil {
			return err
		}
	}
	return h.err
}

func (h *recordingHistory) Close() error { return nil }

func historyHarness(t *testing.T) (*harness, *recordingHistory) {
	t.Helper()
	h := newHarness(t, harnessOpts{authMode: "api_key", masterKey: "sk-master-SPEND"})
	hist := &recordingHistory{}
	h.srv.UseSpendHistory(hist)
	h.gateway = h.srv.Handler()
	return h, hist
}

func historyRequest(target string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("x-gateway-key", "sk-master-SPEND")
	return req
}

// A gateway keeping no history must say so, rather than answering the 404 an
// unknown path gets: the difference between "this gateway cannot do that" and
// "you typed the URL wrong" is the whole message.
func TestSpendHistoryUnconfiguredSaysSo(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", masterKey: "sk-master-SPEND"})
	rec := h.do(t, historyRequest("/spend/history?kind=scope"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "spend_history.dsn") {
		t.Errorf("body = %s, want it to name the setting that enables history", rec.Body.String())
	}
}

// It discloses what every team spent, so it is master-key only like the rest of
// /spend.
func TestSpendHistoryRequiresTheMasterKey(t *testing.T) {
	h, _ := historyHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/spend/history?kind=scope", nil)
	req.Header.Set("x-gateway-key", testVirtualKey)
	if rec := h.do(t, req); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want the request refused without the master key", rec.Code)
	}
}

// A bare call has to answer something useful, because that is what anybody
// tries first.
func TestSpendHistoryDefaultsToThirtyDaysOfScopes(t *testing.T) {
	h, hist := historyHarness(t)
	before := time.Now().UTC()

	rec := h.do(t, historyRequest("/spend/history"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if hist.asked.Kind != spend.KindScope {
		t.Errorf("kind = %q, want scope", hist.asked.Kind)
	}
	if hist.asked.Interval != spend.IntervalDay {
		t.Errorf("interval = %q, want day", hist.asked.Interval)
	}
	if span := hist.asked.To.Sub(hist.asked.From); span < 29*24*time.Hour || span > 31*24*time.Hour {
		t.Errorf("range = %s, want about thirty days", span)
	}
	if hist.asked.To.Before(before) {
		t.Errorf("range ends at %s, before the request was made", hist.asked.To)
	}
}

// A month boundary typed by hand is a date, not a timestamp, and refusing it
// would send whoever is doing chargeback to a converter.
func TestSpendHistoryAcceptsBareDates(t *testing.T) {
	h, hist := historyHarness(t)
	rec := h.do(t, historyRequest("/spend/history?kind=key&subject=hash-a&from=2026-08-01&to=2026-09-01&interval=hour"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	want := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if !hist.asked.From.Equal(want) {
		t.Errorf("from = %s, want %s", hist.asked.From, want)
	}
	if hist.asked.Kind != spend.KindKey || hist.asked.Subject != "hash-a" {
		t.Errorf("query = %+v, want the key it named", hist.asked)
	}
	if hist.asked.Interval != spend.IntervalHour {
		t.Errorf("interval = %q, want hour", hist.asked.Interval)
	}
}

// A query that cannot be answered is the caller's mistake, and must read as
// one: a 400 naming the parameter, not a 500.
func TestSpendHistoryRejectsAnImpossibleRange(t *testing.T) {
	h, _ := historyHarness(t)
	rec := h.do(t, historyRequest("/spend/history?kind=scope&from=2026-09-01&to=2026-08-01"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "after from") {
		t.Errorf("body = %s, want it to name the problem", rec.Body.String())
	}
}

// The totals a report leads with must be the sum of the buckets under them, or
// the two halves of one page disagree.
func TestSpendHistoryTotalsMatchTheBuckets(t *testing.T) {
	h, hist := historyHarness(t)
	day := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	hist.buckets = []spend.Bucket{
		{Start: day, Subject: "acme/payments", Alias: "Payments",
			Totals: spend.Totals{Requests: 10, Cost: 2.5, CacheSavings: 0.5}},
		{Start: day.AddDate(0, 0, 1), Subject: "acme/payments", Alias: "Payments",
			Totals: spend.Totals{Requests: 4, Cost: 1.25, CacheSavings: 0.25}},
	}

	rec := h.do(t, historyRequest("/spend/history?kind=scope&subject=acme/payments"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Buckets           []spend.Bucket `json:"buckets"`
		Count             int            `json:"count"`
		TotalCost         float64        `json:"total_cost"`
		TotalCacheSavings float64        `json:"total_cache_savings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Count != 2 || len(got.Buckets) != 2 {
		t.Fatalf("count = %d over %d buckets, want 2", got.Count, len(got.Buckets))
	}
	if got.TotalCost != 3.75 {
		t.Errorf("total cost = %v, want 3.75", got.TotalCost)
	}
	if got.TotalCacheSavings != 0.75 {
		t.Errorf("total savings = %v, want 0.75", got.TotalCacheSavings)
	}
}

// The export is the artefact chargeback actually opens, so it has to be CSV
// with a header, one line per request, and the figures unrounded.
func TestSpendExportWritesCSV(t *testing.T) {
	h, hist := historyHarness(t)
	hist.rows = []spend.Row{{
		At:         time.Date(2026, 8, 4, 11, 30, 0, 0, time.UTC),
		RequestID:  "req-abc",
		KeyHash:    "hash-a",
		KeyAlias:   "laptop",
		Scope:      "acme/payments",
		ModelGroup: "anthropic-claude",
		Deployment: "anthropic-claude/primary",
		Usage:      core.Usage{InputTokens: 1200, OutputTokens: 300, CacheReadTokens: 900},
		Cost:       0.0123456,
		Billable:   true,
	}}

	rec := h.do(t, historyRequest("/spend/export?kind=scope&subject=acme/payments&from=2026-08-01&to=2026-09-01"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("content-type = %q, want text/csv", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "spend-20260801-20260901.csv") {
		t.Errorf("content-disposition = %q, want the range in the filename", cd)
	}

	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("csv has %d lines, want a header and one row", len(records))
	}
	if records[0][0] != "at" || records[0][11] != "cost" {
		t.Errorf("header = %v, want at first and cost in place", records[0])
	}
	row := records[1]
	if row[3] != "laptop" || row[4] != "acme/payments" {
		t.Errorf("row = %v, want the alias and scope it was charged under", row)
	}
	// Unrounded, because a cent of rounding per request across a month of a
	// fleet's traffic is a number nobody can reconcile.
	if row[11] != "0.0123456" {
		t.Errorf("cost = %q, want the full 0.0123456", row[11])
	}
}
