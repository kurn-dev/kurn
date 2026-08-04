package engine_test

// Regression tests: Queries had no cancellation — a
// long ngram scan could not be interrupted when the client disconnected.
// QueryCtx/LookupCtx poll the context periodically in the scan loops.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kurn-dev/kurn/engine"
)

func bigNgramList(t *testing.T, n int) *engine.List {
	t.Helper()
	l, err := engine.NewList("people", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]engine.Entry, n)
	for i := range entries {
		entries[i] = engine.Entry{ID: fmt.Sprintf("p%06d", i),
			Keys: []string{fmt.Sprintf("veko rima nasol %04d", i%9973)}}
	}
	if err := l.Replace(entries); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestQueryCtxPreCanceledReturnsNil(t *testing.T) {
	l := bigNgramList(t, 5000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c := l.QueryCtx(ctx, "veko rima nasol 1234", engine.QueryOpts{}); c != nil {
		t.Fatalf("pre-canceled query returned %d candidates", len(c))
	}
	// Sanity: the same query with a live context does hit.
	if c := l.QueryCtx(context.Background(), "veko rima nasol 1234", engine.QueryOpts{}); len(c) == 0 {
		t.Fatal("live query found nothing — test data broken")
	}
}

func TestQueryCtxCancelMidScanReturns(t *testing.T) {
	// Every entry shares most grams, and threshold -1 (no floor) makes every
	// gram a generator — the maximal-work scan shape. Cancel shortly after
	// the scan starts; the periodic checks must stop it well before a full
	// uncancelled pass over 200k ordinals × dozens of generators.
	l := bigNgramList(t, 200000)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(2 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	l.QueryCtx(ctx, "veko rima nasol 1234", engine.QueryOpts{Threshold: -1, TopK: -1})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("canceled scan ran %v — cancellation not honored", elapsed)
	}
}

// A scan canceled after an earlier segment already contributed hits must
// return nil — never the partial set as if complete (LookupCtx's nil for
// the canceled segment is indistinguishable from "no hits in that
// segment"). Base carries a guaranteed hit in a tiny segment; the overlay
// is the maximal-work scan the cancel lands in.
func TestQueryCtxCancelAfterPartialSegmentReturnsNil(t *testing.T) {
	l, err := engine.NewList("people", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{{ID: "b1", Keys: []string{"veko rima nasol 1234"}}}); err != nil {
		t.Fatal(err)
	}
	ov := make([]engine.Entry, 300000)
	for i := range ov {
		ov[i] = engine.Entry{ID: fmt.Sprintf("o%06d", i),
			Keys: []string{fmt.Sprintf("veko rima nasol %04d", i%9973)}}
	}
	if err := l.Upsert(ov); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(time.Millisecond) // base scan (tiny) done; overlay scan running
		cancel()
	}()
	if c := l.QueryCtx(ctx, "veko rima nasol 1234", engine.QueryOpts{Threshold: -1, TopK: -1}); c != nil {
		t.Fatalf("canceled query returned %d partial candidates as if complete", len(c))
	}
}
