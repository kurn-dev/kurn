package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func companionCfg(companion bool) engine.ListConfig {
	return engine.ListConfig{
		Analyzer:   engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:      engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, ExactCompanion: companion},
		Filterable: []engine.FilterField{{Name: "tenant", Path: "tenant"}},
	}
}

// companionEntries returns a fresh entry set: p1 and p2 share the analyzed
// key "acme-042" (p2 through its second key), p4's key carries the
// whitespace the analyzer trims.
func companionEntries() []engine.Entry {
	return []engine.Entry{
		{ID: "p1", Keys: []string{"ACME-042"}, Payload: json.RawMessage(`{"tenant":"a"}`)},
		{ID: "p2", Keys: []string{"ACME-024", "acme-042"}, Payload: json.RawMessage(`{"tenant":"b"}`)},
		{ID: "p3", Keys: []string{"ZZ-9"}, Payload: json.RawMessage(`{"tenant":"a"}`)},
		{ID: "p4", Keys: []string{"  acme-402 "}, Payload: json.RawMessage(`{"tenant":"a"}`)},
	}
}

func companionList(t *testing.T, companion bool) *engine.List {
	t.Helper()
	l, err := engine.NewList("parts", companionCfg(companion))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace(companionEntries()); err != nil {
		t.Fatal(err)
	}
	return l
}

func anyOf(t *testing.T, l *engine.List, opts engine.QueryOpts, terms ...string) []engine.Candidate {
	t.Helper()
	p, err := l.PrepareExactAnyOf(terms, opts)
	if err != nil {
		t.Fatal(err)
	}
	c, ver := p.Execute(context.Background())
	if ver != l.Version() {
		t.Fatalf("version %q, list says %q", ver, l.Version())
	}
	return c
}

func candIDs(c []engine.Candidate) string {
	var s []string
	for _, x := range c {
		s = append(s, x.EntryID)
	}
	return strings.Join(s, ",")
}

func TestExactCompanionConfig(t *testing.T) {
	cfg := companionCfg(true)
	cfg.Match.Mode = "exact"
	cfg.Match.Grams = nil
	if _, err := engine.NewList("x", cfg); err == nil || !strings.Contains(err.Error(), "exact_companion") {
		t.Fatalf("exact mode with exact_companion: err = %v, want a refusal naming the field", err)
	}
	with, without := companionList(t, true), companionList(t, false)
	if !with.HasExactIndex() || without.HasExactIndex() {
		t.Fatalf("HasExactIndex: with %v, without %v", with.HasExactIndex(), without.HasExactIndex())
	}
	if !with.Config().Match.ExactCompanion {
		t.Fatal("resolved config lost exact_companion")
	}
	if with.ConfigDigest() == without.ConfigDigest() {
		t.Fatal("exact_companion did not move the config digest")
	}
	if with.BaseExactCompanion() == nil || without.BaseExactCompanion() != nil || with.BaseExact() != nil {
		t.Fatalf("BaseExactCompanion/BaseExact: with %v/%v, without %v", with.BaseExactCompanion() != nil, with.BaseExact() != nil, without.BaseExactCompanion() != nil)
	}
	exact, err := engine.NewList("codes", engine.ListConfig{Analyzer: engine.AnalyzerConfig{Preset: "identifier"}, Match: engine.MatchConfig{Mode: "exact"}})
	if err != nil {
		t.Fatal(err)
	}
	if !exact.HasExactIndex() {
		t.Fatal("an exact-mode list has an exact index")
	}
}

func TestExactAnyOfSemantics(t *testing.T) {
	l := companionList(t, true)

	// Equality under the analyzer; p2 hits through its second key and the
	// attributed key is the one that analyzed to the term.
	c := anyOf(t, l, engine.QueryOpts{}, "Acme-042 ")
	if candIDs(c) != "p1,p2" || c[0].Score != 100 || c[1].Score != 100 || c[0].Key != "ACME-042" || c[1].Key != "acme-042" {
		t.Fatalf("acme-042: %+v", c)
	}
	if string(c[0].Payload) != `{"tenant":"a"}` {
		t.Fatalf("payload: %s", c[0].Payload)
	}
	// Empty-after-analysis and duplicate terms are dropped; the raw key is
	// echoed untrimmed.
	c = anyOf(t, l, engine.QueryOpts{}, "ACME-402", "acme-402", "", "   ")
	if candIDs(c) != "p4" || c[0].Key != "  acme-402 " {
		t.Fatalf("acme-402: %+v", c)
	}
	// An entry reached by two terms appears once.
	if c = anyOf(t, l, engine.QueryOpts{}, "acme-042", "acme-024"); candIDs(c) != "p1,p2" {
		t.Fatalf("two terms, one entry twice: %+v", c)
	}
	// topK cuts; the threshold is not consulted.
	if c = anyOf(t, l, engine.QueryOpts{TopK: 1, Threshold: 0.99}, "acme-042"); candIDs(c) != "p1" {
		t.Fatalf("topK 1: %+v", c)
	}
	if c = anyOf(t, l, engine.QueryOpts{}, "nothing-here"); c != nil {
		t.Fatalf("miss: %+v", c)
	}
	// All terms analyze to nothing: no candidates, no error.
	if c = anyOf(t, l, engine.QueryOpts{}, " ", "\t"); c != nil {
		t.Fatalf("all-empty terms: %+v", c)
	}

	// Bounds and the no-index refusal.
	if _, err := l.PrepareExactAnyOf(nil, engine.QueryOpts{}); err == nil {
		t.Fatal("no terms accepted")
	}
	if _, err := l.PrepareExactAnyOf(make([]string, engine.MaxExactAnyOfTerms+1), engine.QueryOpts{}); err == nil {
		t.Fatal("over-long term list accepted")
	}
	plain := companionList(t, false)
	if _, err := plain.PrepareExactAnyOf([]string{"acme-042"}, engine.QueryOpts{}); err == nil || !strings.Contains(err.Error(), "exact index") {
		t.Fatalf("list without an exact index: err = %v", err)
	}

	// An exact-mode list answers through its primary index.
	exact, err := engine.NewList("codes", engine.ListConfig{Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}}, Match: engine.MatchConfig{Mode: "exact"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := exact.Replace(companionEntries()); err != nil {
		t.Fatal(err)
	}
	if c = anyOf(t, exact, engine.QueryOpts{}, "zz-9", "acme-042"); candIDs(c) != "p1,p2,p3" {
		t.Fatalf("exact-mode any-of: %+v", c)
	}

	// A canceled context yields nothing.
	p, _ := l.PrepareExactAnyOf([]string{"acme-042"}, engine.QueryOpts{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c, _ := p.Execute(ctx); c != nil {
		t.Fatalf("canceled: %+v", c)
	}
}

// The selection is the topK smallest EntryIDs over the whole union: term
// order, base-before-overlay traversal and tombstones must not change it,
// and the retained state (and its charge) is bounded by topK, not by the
// postings walked.
func TestExactAnyOfTopKIsGlobal(t *testing.T) {
	l := companionList(t, true)
	for _, terms := range [][]string{{"acme-402", "acme-042"}, {"acme-042", "acme-402"}} {
		if c := anyOf(t, l, engine.QueryOpts{TopK: 1}, terms...); candIDs(c) != "p1" {
			t.Fatalf("%v topK 1: %+v", terms, c)
		}
		if c := anyOf(t, l, engine.QueryOpts{TopK: 2}, terms...); candIDs(c) != "p1,p2" {
			t.Fatalf("%v topK 2: %+v", terms, c)
		}
		if c := anyOf(t, l, engine.QueryOpts{TopK: 3}, terms...); candIDs(c) != "p1,p2,p4" {
			t.Fatalf("%v topK 3: %+v", terms, c)
		}
	}
	// An overlay entry with the smallest ID is reached last and still wins.
	if err := l.Upsert([]engine.Entry{{ID: "a0", Keys: []string{"ACME-402"}, Payload: json.RawMessage(`{"tenant":"b"}`)}}); err != nil {
		t.Fatal(err)
	}
	if c := anyOf(t, l, engine.QueryOpts{TopK: 1}, "acme-042", "acme-402"); candIDs(c) != "a0" || c[0].Key != "ACME-402" {
		t.Fatalf("overlay smallest: %+v", c)
	}
	if c := anyOf(t, l, engine.QueryOpts{TopK: 2}, "acme-042", "acme-402"); candIDs(c) != "a0,p1" {
		t.Fatalf("overlay topK 2: %+v", c)
	}
	// Filtered selection is global too, and the stats count invocations:
	// p4, then p1 (evicting p4), then a0 (rejected by the filter); p2 can
	// no longer enter once p1 fills the selection, so it is never evaluated.
	filter, _ := engine.ParseTypedFilter([]byte(`{"tenant":"a"}`))
	fq, err := l.PrepareTypedFilteredExactAnyOf([]string{"acme-402", "acme-042"}, engine.QueryOpts{TopK: 1}, filter)
	if err != nil {
		t.Fatal(err)
	}
	if c, _, st, err := fq.ExecuteStats(context.Background()); err != nil || candIDs(c) != "p1" || st.Evaluated != 3 || st.Rejected != 1 {
		t.Fatalf("filtered global topK: %+v %+v %v", c, st, err)
	}

	// A hot key whose base matches are mostly tombstoned: the one survivor
	// is found under TopK 1, and the charge is that of one retained hit,
	// not of the 60 postings walked; unlimited collection is charged for
	// every hit.
	hot, err := engine.NewList("hot", companionCfg(true))
	if err != nil {
		t.Fatal(err)
	}
	var entries []engine.Entry
	for i := 0; i < 60; i++ {
		entries = append(entries, engine.Entry{ID: fmt.Sprintf("h%02d", i), Keys: []string{"HOT"}, Payload: json.RawMessage(`{"tenant":"a"}`)})
	}
	entries = append(entries, engine.Entry{ID: "solo", Keys: []string{"single"}, Payload: json.RawMessage(`{"tenant":"a"}`)})
	if err := hot.Replace(entries); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 59; i++ {
		if err := hot.Delete(fmt.Sprintf("h%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if c := anyOf(t, hot, engine.QueryOpts{TopK: 1}, "hot"); candIDs(c) != "h59" {
		t.Fatalf("tombstone-heavy: %+v", c)
	}
	one, _ := hot.PrepareExactAnyOf([]string{"hot"}, engine.QueryOpts{TopK: 1})
	single, _ := hot.PrepareExactAnyOf([]string{"single"}, engine.QueryOpts{TopK: 1})
	all, _ := hot.PrepareExactAnyOf([]string{"hot"}, engine.QueryOpts{TopK: -1})
	if one.Cost() != single.Cost() {
		t.Fatalf("charge for topK 1 depends on postings walked: %d vs %d", one.Cost(), single.Cost())
	}
	if all.Cost() <= one.Cost() {
		t.Fatalf("unlimited charge %d not above bounded %d", all.Cost(), one.Cost())
	}
	if c, _ := all.Execute(context.Background()); candIDs(c) != "h59" {
		t.Fatalf("unlimited over tombstones: %+v", c)
	}
}

// The admission charge of an any-of lookup must cover everything one
// execution allocates (retained state and transient garbage alike): a
// 20,000-posting key walked under topK 10 and unlimited, with the
// collector off so that HeapAlloc counts every byte.
func TestExactAnyOfAllocationWithinCharge(t *testing.T) {
	const n = 20000
	l, err := engine.NewList("hot", companionCfg(true))
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]engine.Entry, 0, n)
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < n; i++ {
		// Random IDs so the bounded heap evicts, as a real union would.
		entries = append(entries, engine.Entry{ID: fmt.Sprintf("e%08d", rng.Intn(1<<30)), Keys: []string{"HOT"}, Payload: json.RawMessage(`{"tenant":"a"}`)})
	}
	if err := l.Replace(entries); err != nil {
		t.Fatal(err)
	}
	measure := func(p *engine.PreparedQuery) (bytes uint64, hits int) {
		old := debug.SetGCPercent(-1)
		defer debug.SetGCPercent(old)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		c, _ := p.Execute(context.Background())
		runtime.ReadMemStats(&after)
		return after.HeapAlloc - before.HeapAlloc, len(c)
	}
	for _, topK := range []int{10, -1} {
		p, err := l.PrepareExactAnyOf([]string{"hot"}, engine.QueryOpts{TopK: topK})
		if err != nil {
			t.Fatal(err)
		}
		bytes, hits := measure(p)
		want := n
		if topK > 0 {
			want = topK
		}
		if hits != want {
			t.Fatalf("topK %d: %d hits", topK, hits)
		}
		t.Logf("topK %d: allocated %d B for %d hits (%d B/hit), charge %d B", topK, bytes, hits, bytes/uint64(hits), p.Cost())
		if int64(bytes) > p.Cost() {
			t.Fatalf("topK %d: allocated %d B, charged %d B", topK, bytes, p.Cost())
		}
	}
}

func TestExactAnyOfOverlayAndTombstones(t *testing.T) {
	l := companionList(t, true)
	if err := l.Upsert([]engine.Entry{{ID: "p1", Keys: []string{"NEW-1"}, Payload: json.RawMessage(`{"tenant":"a"}`)}}); err != nil {
		t.Fatal(err)
	}
	if c := anyOf(t, l, engine.QueryOpts{}, "acme-042"); candIDs(c) != "p2" {
		t.Fatalf("superseded base entry still matched: %+v", c)
	}
	if c := anyOf(t, l, engine.QueryOpts{}, "new-1"); candIDs(c) != "p1" || c[0].Key != "NEW-1" {
		t.Fatalf("overlay entry: %+v", c)
	}
	if err := l.Delete("p2"); err != nil {
		t.Fatal(err)
	}
	if c := anyOf(t, l, engine.QueryOpts{}, "acme-042"); c != nil {
		t.Fatalf("deleted entry matched: %+v", c)
	}
	if err := l.Compact(); err != nil {
		t.Fatal(err)
	}
	if c := anyOf(t, l, engine.QueryOpts{}, "new-1", "acme-402"); candIDs(c) != "p1,p4" {
		t.Fatalf("after compact: %+v", c)
	}
}

func TestExactAnyOfFiltered(t *testing.T) {
	l := companionList(t, true)
	filter, err := engine.ParseTypedFilter([]byte(`{"tenant":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := l.PrepareTypedFilteredExactAnyOf([]string{"acme-042", "acme-402"}, engine.QueryOpts{}, filter)
	if err != nil {
		t.Fatal(err)
	}
	c, ver, st, err := p.ExecuteStats(context.Background())
	if err != nil || ver != l.Version() {
		t.Fatal(err, ver)
	}
	if candIDs(c) != "p1,p4" || st.Evaluated != 3 || st.Rejected != 1 {
		t.Fatalf("filtered any-of: %+v stats %+v", c, st)
	}
	if string(p.AppliedFilterJSON()) == "" {
		t.Fatal("no applied-filter echo")
	}
	unfiltered, _ := l.PrepareExactAnyOf([]string{"acme-042", "acme-402"}, engine.QueryOpts{})
	if p.Cost() <= unfiltered.Cost() {
		t.Fatalf("filtered cost %d <= unfiltered %d", p.Cost(), unfiltered.Cost())
	}
	miss, _ := l.PrepareExactAnyOf([]string{"none"}, engine.QueryOpts{})
	if unfiltered.Cost() <= miss.Cost() {
		t.Fatalf("cost with hits %d <= cost of a miss %d", unfiltered.Cost(), miss.Cost())
	}
	bad, _ := engine.ParseTypedFilter([]byte(`{"nope":"a"}`))
	if _, err := l.PrepareTypedFilteredExactAnyOf([]string{"acme-042"}, engine.QueryOpts{}, bad); err == nil {
		t.Fatal("undeclared filter name accepted")
	}
}

func TestExactCompanionOrdinaryQueryUnchanged(t *testing.T) {
	with, without := companionList(t, true), companionList(t, false)
	for _, q := range []string{"acme 042", "ACME-024", "zz", "acme-4"} {
		a, b := with.Query(q, engine.QueryOpts{}), without.Query(q, engine.QueryOpts{})
		if len(a) != len(b) {
			t.Fatalf("%q: %d vs %d candidates", q, len(a), len(b))
		}
		for i := range a {
			if a[i].EntryID != b[i].EntryID || a[i].Score != b[i].Score || a[i].Key != b[i].Key {
				t.Fatalf("%q: %+v vs %+v", q, a[i], b[i])
			}
		}
	}
	pa, pb := with.PrepareQuery("acme 042", engine.QueryOpts{}), without.PrepareQuery("acme 042", engine.QueryOpts{})
	if pa.Cost() != pb.Cost() {
		t.Fatalf("ordinary query charge moved with the companion: %d vs %d", pa.Cost(), pb.Cost())
	}
}

func TestExactCompanionPrebuiltIndexes(t *testing.T) {
	source := companionList(t, false)
	idx := source.BaseNgram()
	l, err := engine.NewList("parts", companionCfg(true))
	if err != nil {
		t.Fatal(err)
	}
	// A prebuilt ngram index alone: the companion is rebuilt from the entries.
	if err := l.ReplaceWithIndexInfo(companionEntries(), idx, nil); err != nil {
		t.Fatal(err)
	}
	if c := anyOf(t, l, engine.QueryOpts{}, "acme-042"); candIDs(c) != "p1,p2" {
		t.Fatalf("companion rebuilt from entries: %+v", c)
	}
	if c := l.Query("acme 042", engine.QueryOpts{}); len(c) == 0 {
		t.Fatal("ngram index not installed")
	}
	companion := l.BaseExactCompanion()
	if companion == nil {
		t.Fatal("no companion after ReplaceWithIndexInfo")
	}
	// Both prebuilt.
	l2, _ := engine.NewList("parts", companionCfg(true))
	if err := l2.ReplaceWithIndexesInfo(companionEntries(), idx, companion, nil); err != nil {
		t.Fatal(err)
	}
	if c := anyOf(t, l2, engine.QueryOpts{}, "acme-402"); candIDs(c) != "p4" {
		t.Fatalf("prebuilt companion: %+v", c)
	}
	// Presence must match the configuration.
	if err := l2.ReplaceWithIndexesInfo(companionEntries(), idx, nil, nil); err == nil {
		t.Fatal("companion list accepted a missing companion")
	}
	plain, _ := engine.NewList("parts", companionCfg(false))
	if err := plain.ReplaceWithIndexesInfo(companionEntries(), idx, companion, nil); err == nil {
		t.Fatal("plain list accepted a companion")
	}
	// A companion with more ordinals than entries is a mismatched pair.
	if err := l2.ReplaceWithIndexesInfo(companionEntries()[:2], idx, companion, nil); err == nil {
		t.Fatal("companion past the entries accepted")
	}
}

func TestStoreExactCompanionReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("parts", companionCfg(true)); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("parts", companionEntries()); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("parts", []engine.Entry{{ID: "p5", Keys: []string{"Late-5"}, Payload: json.RawMessage(`{"tenant":"a"}`)}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	// Reopen: the base comes back through base.idx (ngram only, so the
	// companion is rebuilt), the overlay through journal replay.
	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	l, ok := st2.List("parts")
	if !ok {
		t.Fatal("list lost")
	}
	if !l.Config().Match.ExactCompanion {
		t.Fatal("config.json lost exact_companion")
	}
	if c := anyOf(t, l, engine.QueryOpts{}, "acme-042", "late-5"); candIDs(c) != "p1,p2,p5" {
		t.Fatalf("after reopen: %+v", c)
	}
	if c := l.Query("acme 042", engine.QueryOpts{}); len(c) == 0 {
		t.Fatal("ngram query after reopen")
	}
}
