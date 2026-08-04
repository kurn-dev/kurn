package engine_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func personList(t *testing.T, entries ...engine.Entry) *engine.List {
	t.Helper()
	l, err := engine.NewList("people", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, Threshold: 0.6, TopK: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace(entries); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestQueryBasics(t *testing.T) {
	l := personList(t,
		engine.Entry{ID: "p1", Keys: []string{"Elena Vasquez", "Vasquez, Elena"}, Payload: json.RawMessage(`{"n":1}`)},
		engine.Entry{ID: "p2", Keys: []string{"Marcus Chen"}},
	)
	cands := l.Query("Elena Wasquez", engine.QueryOpts{})
	if len(cands) == 0 || cands[0].EntryID != "p1" {
		t.Fatalf("cands = %+v, want p1 first", cands)
	}
	if string(cands[0].Payload) != `{"n":1}` {
		t.Errorf("payload = %s, want verbatim", cands[0].Payload)
	}
	if cands[0].Key == "" {
		t.Errorf("want attributed key, got empty")
	}
}

// The symmetry property: index a key, query the identical string -> score 100.
func TestBuildQuerySymmetry(t *testing.T) {
	keys := []string{"Dr. José-María O'Connor", "ACME Corp — Zürich", "vasquez elena"}
	entries := make([]engine.Entry, len(keys))
	for i, k := range keys {
		entries[i] = engine.Entry{ID: string(rune('a' + i)), Keys: []string{k}}
	}
	l := personList(t, entries...)
	for i, k := range keys {
		cands := l.Query(k, engine.QueryOpts{})
		if len(cands) == 0 || cands[0].EntryID != entries[i].ID || cands[0].Score != 100 {
			t.Errorf("symmetry broken for %q: %+v", k, cands)
		}
	}
}

func TestQueryOverrides(t *testing.T) {
	l := personList(t,
		engine.Entry{ID: "p1", Keys: []string{"Elena Vasquez"}},
		engine.Entry{ID: "p2", Keys: []string{"Elena Sandoval"}},
	)
	// Query "elena vasquez": full coverage of p1, partial coverage of p2
	// (shared "elena" grams only). Scoring is query-gram coverage, so a
	// query contained in every key (e.g. just "elena") would score 100
	// everywhere and thresholds could not discriminate.
	loose := l.Query("elena vasquez", engine.QueryOpts{Threshold: 0.3})
	strict := l.Query("elena vasquez", engine.QueryOpts{Threshold: 0.95})
	if len(loose) <= len(strict) {
		t.Errorf("loose (%d) should out-hit strict (%d)", len(loose), len(strict))
	}
	one := l.Query("elena vasquez", engine.QueryOpts{Threshold: 0.3, TopK: 1})
	if len(one) != 1 {
		t.Errorf("TopK=1 gave %d", len(one))
	}
}

func TestExactModeList(t *testing.T) {
	l, err := engine.NewList("skus", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "identifier"},
		Match:    engine.MatchConfig{Mode: "exact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{{ID: "s1", Keys: []string{"ACME-042"}}}); err != nil {
		t.Fatal(err)
	}
	if c := l.Query("  acme-042 ", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "s1" || c[0].Score != 100 {
		t.Errorf("exact query = %+v", c)
	}
	if c := l.Query("acme-999", engine.QueryOpts{}); len(c) != 0 {
		t.Errorf("miss returned %+v", c)
	}
}

// Duplicate IDs within one Replace batch: later duplicates win (upsert
// semantics) — exactly one candidate, with the LAST occurrence's key/payload.
func TestReplaceDupIDsLastWins(t *testing.T) {
	// The loser's key is gram-disjoint from the winner's: a shared gram
	// would let the winner score 100 on the loser's-key query below.
	l := personList(t,
		engine.Entry{ID: "p1", Keys: []string{"Toni Brook"}, Payload: json.RawMessage(`{"v":1}`)},
		engine.Entry{ID: "p1", Keys: []string{"Elena Vasquez"}, Payload: json.RawMessage(`{"v":2}`)},
	)
	cands := l.Query("Elena Vasquez", engine.QueryOpts{})
	if len(cands) != 1 {
		t.Fatalf("cands = %+v, want exactly one", cands)
	}
	if cands[0].EntryID != "p1" || cands[0].Key != "Elena Vasquez" || string(cands[0].Payload) != `{"v":2}` {
		t.Errorf("cand = %+v, want last occurrence's key and payload", cands[0])
	}
	if c := l.Query("Toni Brook", engine.QueryOpts{}); len(c) != 0 {
		t.Errorf("first duplicate's key still matches: %+v", c)
	}
	if entries, _, _ := l.Stats(); entries != 1 {
		t.Errorf("entries = %d, want 1", entries)
	}
}

func TestStatsAndVersion(t *testing.T) {
	l := personList(t,
		engine.Entry{ID: "p1", Keys: []string{"Elena Vasquez"}},
		engine.Entry{ID: "p2", Keys: []string{"Marcus Chen"}},
	)
	if e, o, tomb := l.Stats(); e != 2 || o != 0 || tomb != 0 {
		t.Errorf("stats = %d,%d,%d, want 2,0,0", e, o, tomb)
	}
	v1 := l.Version()
	if v1 == "" {
		t.Error("empty version")
	}
	// Same-sized Replace must still produce a distinct version stamp.
	if err := l.Replace([]engine.Entry{
		{ID: "p3", Keys: []string{"John Smith"}},
		{ID: "p4", Keys: []string{"Jane Doe"}},
	}); err != nil {
		t.Fatal(err)
	}
	if v2 := l.Version(); v2 == v1 {
		t.Errorf("same-sized Replaces produced identical version %q", v1)
	}
	if e, o, tomb := l.Stats(); e != 2 || o != 0 || tomb != 0 {
		t.Errorf("stats after replace = %d,%d,%d, want 2,0,0", e, o, tomb)
	}
}

// Exact mode: two entries sharing a key are both returned at 100, ID-ordered.
func TestExactSharedKey(t *testing.T) {
	l, err := engine.NewList("skus", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "identifier"},
		Match:    engine.MatchConfig{Mode: "exact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{
		{ID: "b", Keys: []string{"shared"}},
		{ID: "a", Keys: []string{"shared"}},
	}); err != nil {
		t.Fatal(err)
	}
	c := l.Query("shared", engine.QueryOpts{})
	if len(c) != 2 || c[0].EntryID != "a" || c[1].EntryID != "b" || c[0].Score != 100 || c[1].Score != 100 {
		t.Errorf("shared-key query = %+v, want a then b, both 100", c)
	}
}

func TestNewListValidation(t *testing.T) {
	if _, err := engine.NewList("x", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "identifier"},
		Match:    engine.MatchConfig{Mode: "bogus"},
	}); err == nil {
		t.Error("want error for unknown match mode")
	}
	l, err := engine.NewList("x", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := l.Config().Match
	if !slices.Equal(m.Grams, []int{2, 3}) || m.Threshold != 0.6 || m.TopK != 100 {
		t.Errorf("defaults not filled: %+v, want Grams [2 3], Threshold 0.6, TopK 100", m)
	}
	// Invalid gram sizes must fail here with an error, not panic later in
	// ngram.NewBuilder when the first segment is built.
	for _, grams := range [][]int{{0}, {-1}, {2, 0}} {
		if _, err := engine.NewList("x", engine.ListConfig{
			Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
			Match:    engine.MatchConfig{Mode: "ngram", Grams: grams},
		}); err == nil {
			t.Errorf("NewList with Grams=%v: want error, got nil", grams)
		}
	}
}

// Lock-free query path: 8 readers race one writer doing periodic Replaces.
// Every Replace keeps p1, so every query must find it. Run under -race.
func TestConcurrentQueryReplace(t *testing.T) {
	l := personList(t, engine.Entry{ID: "p1", Keys: []string{"Elena Vasquez"}})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < 100; i++ {
			err := l.Replace([]engine.Entry{
				{ID: "p1", Keys: []string{"Elena Vasquez"}},
				{ID: "p2", Keys: []string{"Marcus Chen"}},
			})
			if err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				c := l.Query("Elena Vasquez", engine.QueryOpts{})
				if len(c) == 0 || c[0].EntryID != "p1" || c[0].Score != 100 {
					t.Errorf("concurrent query gave %+v", c)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// Allocation/latency regression baseline for the query hot path.
func BenchmarkQuery(b *testing.B) {
	l, err := engine.NewList("people", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, Threshold: 0.6, TopK: 100},
	})
	if err != nil {
		b.Fatal(err)
	}
	first := []string{"elena", "marcus", "john", "jane", "dana", "omar", "nils", "mira", "tomas", "kiran"}
	last := []string{"vasquez", "chen", "smith", "doe", "kovak", "reyes", "berg", "solano", "okafor", "lindqvist"}
	var entries []engine.Entry
	for i, f := range first {
		for j, ln := range last {
			entries = append(entries, engine.Entry{
				ID:   fmt.Sprintf("p%d-%d", i, j),
				Keys: []string{f + " " + ln},
			})
		}
	}
	if err := l.Replace(entries); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if c := l.Query("elena vasquze", engine.QueryOpts{}); len(c) == 0 {
			b.Fatal("no hits")
		}
	}
}
