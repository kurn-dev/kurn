package engine_test

// Filtered-query recall proofs: a filtered query must return the top-K of
// the FILTERED stream — the pre-cut evaluation is what makes this true,
// and the naive reference (collect all, decode-filter, sort, cut) pins it
// across floods, masks, overlays, exact hot runs, and parent fallback.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kurn-dev/kurn/engine"
)

func pEntry(id, key, program string) engine.Entry {
	return engine.Entry{
		ID:      id,
		Keys:    []string{key},
		Payload: json.RawMessage(fmt.Sprintf(`{"program":%q}`, program)),
	}
}

func filterableCfg() engine.ListConfig {
	return engine.ListConfig{
		Analyzer:   engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:      engine.MatchConfig{Mode: "ngram"},
		Filterable: []engine.FilterField{{Name: "program", Path: "program"}},
	}
}

// refFiltered is the naive reference: collect EVERYTHING (unlimited),
// decode-filter, then the engine's documented final order (score desc,
// EntryID asc), then the cut. Ord-order nuances are avoided by distinct
// scores except where a case is specifically about ties.
func refFiltered(t *testing.T, l *engine.List, q string, topK int, filter map[string]string) []engine.Candidate {
	t.Helper()
	all, _ := l.QueryVersioned(context.Background(), q, engine.QueryOpts{TopK: -1})
	var out []engine.Candidate
	for _, c := range all {
		ok, err := refMatch(c.Payload, filter)
		if err != nil {
			t.Fatalf("reference decode: %v", err)
		}
		if ok {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].EntryID < out[j].EntryID
	})
	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	return out
}

func refMatch(payload json.RawMessage, filter map[string]string) (bool, error) {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return false, err
	}
	names := make([]string, 0, len(filter))
	for n := range filter {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v, ok := m[n]
		if !ok {
			return false, nil
		}
		s, ok := v.(string)
		if !ok || s != filter[n] {
			return false, nil
		}
	}
	return true, nil
}

func ids(cands []engine.Candidate) string {
	var parts []string
	for _, c := range cands {
		parts = append(parts, fmt.Sprintf("%s:%v", c.EntryID, c.Score))
	}
	return strings.Join(parts, ",")
}

func TestFilteredQueryRecall(t *testing.T) {
	l, err := engine.NewList("sanctions", filterableCfg())
	if err != nil {
		t.Fatal(err)
	}
	var entries []engine.Entry
	// 200 entries sharing the key grams with distinct scores... equal keys
	// give equal scores; instead vary one letter so scores differ slightly,
	// and interleave SDN (wanted) sparsely at high ranks.
	for i := 0; i < 200; i++ {
		prog := "OTHER"
		if i%37 == 0 { // wanted hits at ord 0, 37, 74, 111, 148, 185
			prog = "SDN"
		}
		entries = append(entries, pEntry(fmt.Sprintf("p%03d", i), fmt.Sprintf("alexander zakharen%d", i%10), prog))
	}
	if err := l.Replace(entries); err != nil {
		t.Fatal(err)
	}

	got, ver, err := l.QueryFilteredCtx(context.Background(), "alexander zakharen3", engine.QueryOpts{TopK: 10}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	if ver == "" {
		t.Fatal("filtered query returned no version")
	}
	want := refFiltered(t, l, "alexander zakharen3", 10, map[string]string{"program": "SDN"})
	if ids(got) != ids(want) {
		t.Fatalf("filtered != reference:\n got %s\nwant %s", ids(got), ids(want))
	}
	if len(got) == 0 || len(got) > 10 {
		t.Fatalf("filtered count %d outside (0,10]", len(got))
	}
	for _, c := range got {
		if !strings.Contains(string(c.Payload), "SDN") {
			t.Fatalf("unfiltered payload in results: %s", c.Payload)
		}
	}
}

// The rank proof: the filtered match sits BELOW the unfiltered top-K.
// Post-cut filtering returns nothing here; pre-cut finds it.
func TestFilteredRankBelowUnfilteredTopK(t *testing.T) {
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
	got, _, err := l.QueryFilteredCtx(context.Background(), "same name", engine.QueryOpts{TopK: 10}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].EntryID != "winner" {
		t.Fatalf("the rank-151 filtered match was not found: %s", ids(got))
	}
}

// Tombstoned entries must not pass the filtered predicate (live-mask fold).
func TestFilteredMaskTombstones(t *testing.T) {
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
	got, _, err := l.QueryFilteredCtx(context.Background(), "dana kovak", engine.QueryOpts{}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("tombstoned entry passed the filter: %s", ids(got))
	}
}

// Exact mode: a fully filtered level must descend to the parent, like a
// fully tombstoned one.
func TestFilteredExactParentFallback(t *testing.T) {
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
	got, _, err := l.QueryFilteredCtx(context.Background(), "bad.example.com", engine.QueryOpts{}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].EntryID != "parent" || got[0].Score != 90 {
		t.Fatalf("fully-filtered level did not descend to parent: %s", ids(got))
	}
}

// Overlay upserts supersede base entries (the base hit is tombstoned):
// the filtered predicate must evaluate the OVERLAY copy's payload, and
// the stale base payload must never contribute evidence.
func TestFilteredOverlaySupersedesBase(t *testing.T) {
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
	}); err != nil {
		t.Fatal(err)
	}
	// Re-upsert p1 with a changed payload: base said SDN, overlay says OTHER.
	if err := st.Upsert("people", []engine.Entry{
		pEntry("p1", "dana kovak", "OTHER"),
	}); err != nil {
		t.Fatal(err)
	}
	l, ok := st.List("people")
	if !ok {
		t.Fatal("list missing")
	}
	got, _, err := l.QueryFilteredCtx(context.Background(), "dana kovak", engine.QueryOpts{}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("stale base payload passed the filter after upsert: %s", ids(got))
	}
	got, _, err = l.QueryFilteredCtx(context.Background(), "dana kovak", engine.QueryOpts{}, map[string]string{"program": "OTHER"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].EntryID != "p1" {
		t.Fatalf("overlay copy not found under its new payload: %s", ids(got))
	}
}

// Exact hot-run walk + cancellation: one key shared by 50k entries, all
// failing the filter except the LAST ordinal. A completed walk must find
// it; a mid-walk cancellation must return nil via the 512-ordinal poll —
// the outcome distinguishes the two (a missing poll would return the hit).
func TestFilteredExactHotRunAndCancellation(t *testing.T) {
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
	// Live: the walk completes and finds the last-ordinal match.
	got, _, err := l.QueryFilteredCtx(context.Background(), "hot key", engine.QueryOpts{}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].EntryID != "winner" {
		t.Fatalf("hot-run walk did not find the last-ordinal match: %s", ids(got))
	}
	// Canceled mid-walk: nil, via the poll.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(2 * time.Millisecond)
		cancel()
	}()
	got, _, err = l.QueryFilteredCtx(ctx, "hot key", engine.QueryOpts{}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("canceled filtered walk returned candidates — poll not firing")
	}
}

func TestFilteredErrorsAndNoFilterParity(t *testing.T) {
	l, err := engine.NewList("x", filterableCfg())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{pEntry("p1", "dana kovak", "SDN")}); err != nil {
		t.Fatal(err)
	}
	// Undeclared name: fail closed.
	if _, _, err := l.QueryFilteredCtx(context.Background(), "dana kovak", engine.QueryOpts{}, map[string]string{"progrma": "SDN"}); err == nil {
		t.Fatal("undeclared filter name accepted")
	}
	// Malformed payload aborts a filtered query (never a silent drop).
	bad, err := engine.NewList("bad", filterableCfg())
	if err != nil {
		t.Fatal(err)
	}
	if err := bad.Replace([]engine.Entry{
		pEntry("ok", "dana kovak", "SDN"),
		{ID: "bad", Keys: []string{"dana kovak"}, Payload: json.RawMessage(`{"program":`)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bad.QueryFilteredCtx(context.Background(), "dana kovak", engine.QueryOpts{TopK: -1}, map[string]string{"program": "SDN"}); err == nil {
		t.Fatal("malformed payload did not abort the filtered query")
	}
	// No filter: identical results to QueryVersioned.
	a, va := l.QueryVersioned(context.Background(), "dana kovak", engine.QueryOpts{})
	b, vb, err := l.QueryFilteredCtx(context.Background(), "dana kovak", engine.QueryOpts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ids(a) != ids(b) || va != vb {
		t.Fatal("nil filter changed the no-filter path")
	}
	p, err := l.PrepareFilteredQuery("dana kovak", engine.QueryOpts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.AppliedFilter() != nil {
		t.Fatal("no-filter prepare reports an applied filter")
	}
}
