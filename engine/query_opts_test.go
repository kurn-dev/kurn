package engine_test

// Regression tests: Negative QueryOpts values are the
// documented sentinel for "explicitly zero" (no threshold floor / unlimited
// top-K) — pre-fix they silently meant "use the list default", making
// threshold=0 and topk=0 inexpressible per-query on ngram lists. Also pins
// the exact-mode collection bound: a hot-key list must return at most topK
// candidates.

import (
	"fmt"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func ngramList(t *testing.T, topK int) *engine.List {
	t.Helper()
	l, err := engine.NewList("people", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match: engine.MatchConfig{
			Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, TopK: topK,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{
		{ID: "p1", Keys: []string{"elena vasquez"}},
		{ID: "p2", Keys: []string{"elena vasqueza"}},
		{ID: "p3", Keys: []string{"elena vasquezov"}},
	}); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestQueryOptsNegativeTopKIsUnlimited(t *testing.T) {
	l := ngramList(t, 2) // list default topk = 2

	if got := len(l.Query("elena vasquez", engine.QueryOpts{})); got != 2 {
		t.Fatalf("default topk: %d candidates, want the list default 2", got)
	}
	if got := len(l.Query("elena vasquez", engine.QueryOpts{TopK: 1})); got != 1 {
		t.Fatalf("topk 1: %d candidates, want 1", got)
	}
	if got := len(l.Query("elena vasquez", engine.QueryOpts{TopK: -1})); got != 3 {
		t.Fatalf("topk -1 (unlimited sentinel): %d candidates, want all 3", got)
	}
}

func TestQueryOptsNegativeThresholdIsNoFloor(t *testing.T) {
	l, err := engine.NewList("people", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match: engine.MatchConfig{
			Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, Threshold: 0.9,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// p1 covers the query fully (≈100); p2 shares only the "elena" half
	// (≈50) — under the list's 0.9 floor, above 0.
	if err := l.Replace([]engine.Entry{
		{ID: "p1", Keys: []string{"elena vasquez"}},
		{ID: "p2", Keys: []string{"elena marlow"}},
	}); err != nil {
		t.Fatal(err)
	}

	def := len(l.Query("elena vasquez", engine.QueryOpts{}))
	if def != 1 {
		t.Fatalf("default threshold 0.9: %d candidates, want 1 (p2 hidden)", def)
	}
	if got := len(l.Query("elena vasquez", engine.QueryOpts{Threshold: -1})); got != 2 {
		t.Fatalf("threshold -1 (no-floor sentinel): %d candidates, want both", got)
	}
	// Zero still means "list default" (unchanged, documented).
	if got := len(l.Query("elena vasquez", engine.QueryOpts{Threshold: 0})); got != 1 {
		t.Fatalf("threshold 0: %d candidates, want the default behavior (1)", got)
	}
}

// An exact-mode hot key (many entries sharing one analyzed key) must not
// collect the full hit set when a top-K applies: at most topK candidates
// come back. (Which equal-scored ties survive follows collection order —
// the same documented stance as the ngram per-segment cut.)
func TestExactHotKeyCollectionBounded(t *testing.T) {
	l, err := engine.NewList("codes", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "exact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]engine.Entry, 300)
	for i := range entries {
		entries[i] = engine.Entry{ID: fmt.Sprintf("e%03d", i), Keys: []string{"hot"}}
	}
	if err := l.Replace(entries); err != nil {
		t.Fatal(err)
	}
	if got := len(l.Query("hot", engine.QueryOpts{TopK: 100})); got != 100 {
		t.Fatalf("topk 100 on 300 exact hits: %d candidates, want 100", got)
	}
	// cfg TopK 0 on exact lists stays unlimited (deliberate, item 20).
	if got := len(l.Query("hot", engine.QueryOpts{})); got != 300 {
		t.Fatalf("no topk on exact list: %d candidates, want all 300", got)
	}
}
