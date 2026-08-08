package engine_test

// FilterStats proofs: the counters report exactly the live,
// score-qualified predicate invocations an execution performed, per the
// documented semantics — tombstones and unreached candidates never
// count, failed or canceled runs report zero, and the stats path
// changes nothing about the candidates themselves (differential against
// Execute). Counts asserted here are exact: fixtures are built so the
// scan order and evaluation set are deterministic for the pinned
// snapshot and request.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/kurn-dev/kurn/engine"
)

func execStats(t *testing.T, l *engine.List, q string, opts engine.QueryOpts, filter map[string]string) ([]engine.Candidate, engine.FilterStats) {
	t.Helper()
	p, err := l.PrepareFilteredQuery(q, opts, filter)
	if err != nil {
		t.Fatal(err)
	}
	cands, _, fst, err := p.ExecuteStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cands, fst
}

// Ngram: every floor-passing live candidate is evaluated exactly once,
// pre-cut — 150 rejections plus the one match, regardless of topK.
func TestFilterStatsNgramCounts(t *testing.T) {
	l, err := engine.NewList("flood", filterableCfg())
	if err != nil {
		t.Fatal(err)
	}
	var entries []engine.Entry
	for i := 0; i < 150; i++ {
		entries = append(entries, pEntry(fmt.Sprintf("p%03d", i), "same name", "OTHER"))
	}
	entries = append(entries, pEntry("winner", "same name", "SDN"))
	if err := l.Replace(entries); err != nil {
		t.Fatal(err)
	}
	cands, fst := execStats(t, l, "same name", engine.QueryOpts{TopK: 10}, map[string]string{"program": "SDN"})
	if len(cands) != 1 || cands[0].EntryID != "winner" {
		t.Fatalf("unexpected candidates: %s", ids(cands))
	}
	if fst.Evaluated != 151 || fst.Rejected != 150 {
		t.Fatalf("ngram stats = %+v, want {Evaluated:151 Rejected:150}", fst)
	}
}

// Exact hot run: the walk stops once topK survivors are collected, so
// later ordinals are never evaluated — early stopping is visible in the
// counts (7 evaluated of 10 stored: five rejections, then two accepts).
func TestFilterStatsExactEarlyStop(t *testing.T) {
	cfg := engine.ListConfig{
		Analyzer:   engine.AnalyzerConfig{Steps: []string{"lowercase"}},
		Match:      engine.MatchConfig{Mode: "exact"},
		Filterable: []engine.FilterField{{Name: "program", Path: "program"}},
	}
	l, err := engine.NewList("hot", cfg)
	if err != nil {
		t.Fatal(err)
	}
	var entries []engine.Entry
	for i := 0; i < 10; i++ {
		prog := "OTHER"
		if i >= 5 {
			prog = "SDN"
		}
		entries = append(entries, pEntry(fmt.Sprintf("p%d", i), "hot key", prog))
	}
	if err := l.Replace(entries); err != nil { // Replace keeps caller ord order
		t.Fatal(err)
	}
	cands, fst := execStats(t, l, "hot key", engine.QueryOpts{TopK: 2}, map[string]string{"program": "SDN"})
	if len(cands) != 2 {
		t.Fatalf("want 2 candidates, got %s", ids(cands))
	}
	if fst.Evaluated != 7 || fst.Rejected != 5 {
		t.Fatalf("exact early-stop stats = %+v, want {Evaluated:7 Rejected:5}", fst)
	}
}

// Exact parent fallback: the fully-rejected sub level's evaluation is
// real work and stays counted when the walk descends to the parent.
func TestFilterStatsParentFallbackAccumulates(t *testing.T) {
	cfg := engine.ListConfig{
		Analyzer:   engine.AnalyzerConfig{Steps: []string{"lowercase"}},
		Match:      engine.MatchConfig{Mode: "exact", Fallback: "parent_domain"},
		Filterable: []engine.FilterField{{Name: "program", Path: "program"}},
	}
	l, err := engine.NewList("domains", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{
		pEntry("sub", "bad.example.com", "OTHER"),
		pEntry("parent", "example.com", "SDN"),
	}); err != nil {
		t.Fatal(err)
	}
	cands, fst := execStats(t, l, "bad.example.com", engine.QueryOpts{}, map[string]string{"program": "SDN"})
	if len(cands) != 1 || cands[0].EntryID != "parent" {
		t.Fatalf("fallback candidates wrong: %s", ids(cands))
	}
	if fst.Evaluated != 2 || fst.Rejected != 1 {
		t.Fatalf("fallback stats = %+v, want {Evaluated:2 Rejected:1}", fst)
	}
}

// Tombstoned entries are masked BEFORE the predicate: a deleted entry
// contributes to neither counter. The surviving rejection is also the
// filtered-empty-success shape: stats present, zero candidates, no error.
func TestFilterStatsTombstonesUncountedAndEmptySuccess(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateList("people", filterableCfg()); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("people", []engine.Entry{
		pEntry("p1", "dana kovak", "SDN"),
		pEntry("p2", "dana kovak", "OTHER"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete("people", "p1"); err != nil {
		t.Fatal(err)
	}
	l, ok := st.List("people")
	if !ok {
		t.Fatal("list missing")
	}
	cands, fst := execStats(t, l, "dana kovak", engine.QueryOpts{}, map[string]string{"program": "SDN"})
	if len(cands) != 0 {
		t.Fatalf("tombstoned entry surfaced: %s", ids(cands))
	}
	if fst.Evaluated != 1 || fst.Rejected != 1 {
		t.Fatalf("tombstone stats = %+v, want {Evaluated:1 Rejected:1} (p1 masked, p2 rejected)", fst)
	}
}

// Overlay supersedes base: the masked base copy is not evaluated; the
// overlay copy is evaluated exactly once.
func TestFilterStatsOverlayCopyOnly(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateList("people", filterableCfg()); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("people", []engine.Entry{pEntry("p1", "dana kovak", "SDN")}); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("people", []engine.Entry{pEntry("p1", "dana kovak", "OTHER")}); err != nil {
		t.Fatal(err)
	}
	l, ok := st.List("people")
	if !ok {
		t.Fatal("list missing")
	}
	cands, fst := execStats(t, l, "dana kovak", engine.QueryOpts{}, map[string]string{"program": "OTHER"})
	if len(cands) != 1 || cands[0].EntryID != "p1" {
		t.Fatalf("overlay copy not matched: %s", ids(cands))
	}
	if fst.Evaluated != 1 || fst.Rejected != 0 {
		t.Fatalf("overlay stats = %+v, want {Evaluated:1 Rejected:0} (base copy masked, not evaluated)", fst)
	}
}

// A failed execution (malformed stored payload) reports zero stats:
// partial counts must never read as successful evidence.
func TestFilterStatsZeroOnMalformedAbort(t *testing.T) {
	l, err := engine.NewList("bad", filterableCfg())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{
		pEntry("ok", "dana kovak", "SDN"),
		{ID: "bad", Keys: []string{"dana kovak"}, Payload: json.RawMessage(`{"program":`)},
	}); err != nil {
		t.Fatal(err)
	}
	p, err := l.PrepareFilteredQuery("dana kovak", engine.QueryOpts{TopK: -1}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, fst, err := p.ExecuteStats(context.Background())
	if err == nil {
		t.Fatal("malformed payload did not abort")
	}
	if fst != (engine.FilterStats{}) {
		t.Fatalf("failed execution leaked stats: %+v", fst)
	}
}

// A canceled execution reports nil candidates and zero stats — the
// mid-walk poll fires on the hot run (same fixture shape as the
// cancellation recall test) and the partial counts are discarded.
func TestFilterStatsZeroOnCancellation(t *testing.T) {
	cfg := engine.ListConfig{
		Analyzer:   engine.AnalyzerConfig{Steps: []string{"lowercase"}},
		Match:      engine.MatchConfig{Mode: "exact"},
		Filterable: []engine.FilterField{{Name: "program", Path: "program"}},
	}
	l, err := engine.NewList("hot", cfg)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]engine.Entry, 50000)
	for i := range entries {
		entries[i] = pEntry(fmt.Sprintf("p%d", i), "hot key", "OTHER")
	}
	entries[len(entries)-1] = pEntry("winner", "hot key", "SDN")
	if err := l.Replace(entries); err != nil {
		t.Fatal(err)
	}
	p, err := l.PrepareFilteredQuery("hot key", engine.QueryOpts{}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(2 * time.Millisecond)
		cancel()
	}()
	cands, _, fst, err := p.ExecuteStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cands != nil {
		t.Fatal("canceled execution returned candidates")
	}
	if fst != (engine.FilterStats{}) {
		t.Fatalf("canceled execution leaked partial stats: %+v", fst)
	}
}

// Differential: ExecuteStats returns byte-identical candidates and
// version to Execute for the same query, and re-executing the SAME
// prepared query yields the same stats both times — no retained
// per-run state.
func TestFilterStatsDifferentialAndRepeatable(t *testing.T) {
	l, err := engine.NewList("sanctions", filterableCfg())
	if err != nil {
		t.Fatal(err)
	}
	var entries []engine.Entry
	for i := 0; i < 200; i++ {
		prog := "OTHER"
		if i%37 == 0 {
			prog = "SDN"
		}
		entries = append(entries, pEntry(fmt.Sprintf("p%03d", i), fmt.Sprintf("alexander zakharen%d", i%10), prog))
	}
	if err := l.Replace(entries); err != nil {
		t.Fatal(err)
	}
	filter := map[string]string{"program": "SDN"}
	opts := engine.QueryOpts{TopK: 10}
	pa, err := l.PrepareFilteredQuery("alexander zakharen3", opts, filter)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := l.PrepareFilteredQuery("alexander zakharen3", opts, filter)
	if err != nil {
		t.Fatal(err)
	}
	a, va, err := pa.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, vb, fst1, err := pb.ExecuteStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ids(a) != ids(b) || va != vb {
		t.Fatalf("ExecuteStats changed the answer:\n Execute      %s (%s)\n ExecuteStats %s (%s)", ids(a), va, ids(b), vb)
	}
	if fst1.Evaluated == 0 || fst1.Rejected == 0 {
		t.Fatalf("implausible stats for the flood fixture: %+v", fst1)
	}
	b2, _, fst2, err := pb.ExecuteStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ids(b) != ids(b2) || fst1 != fst2 {
		t.Fatalf("re-execution drifted: stats %+v then %+v", fst1, fst2)
	}
}

// The no-filter prepared path reports zero stats and identical
// candidates — the stats member is meaningless without a filter and the
// engine keeps it exactly zero.
func TestFilterStatsNoFilterZero(t *testing.T) {
	l, err := engine.NewList("x", filterableCfg())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{pEntry("p1", "dana kovak", "SDN")}); err != nil {
		t.Fatal(err)
	}
	p, err := l.PrepareFilteredQuery("dana kovak", engine.QueryOpts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cands, _, fst, err := p.ExecuteStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("no-filter path lost the candidate: %s", ids(cands))
	}
	if fst != (engine.FilterStats{}) {
		t.Fatalf("no-filter execution reported stats: %+v", fst)
	}
}
