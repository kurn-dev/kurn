package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func exactCancelEntry(id, key, program string) Entry {
	return Entry{
		ID:      id,
		Keys:    []string{key},
		Payload: json.RawMessage(fmt.Sprintf(`{"program":%q}`, program)),
	}
}

func exactCancelList(t *testing.T, fallback string) *List {
	t.Helper()
	l, err := NewList("hot", ListConfig{
		Analyzer:   AnalyzerConfig{Steps: []string{"lowercase"}},
		Match:      MatchConfig{Mode: "exact", Fallback: fallback},
		Filterable: []FilterField{{Name: "program", Path: "program"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func cancelFilteredAtFirstExactPoll(t *testing.T, l *List, q string) {
	t.Helper()
	p, err := l.PrepareFilteredQuery(q, QueryOpts{TopK: -1}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var polls []int
	testHookExactCancelPoll = func(walked, every int) {
		polls = append(polls, walked)
		if every != exactFilteredCancelCheckEvery {
			t.Fatalf("filtered exact poll interval = %d, want %d", every, exactFilteredCancelCheckEvery)
		}
		if len(polls) == 1 {
			cancel()
		}
	}
	defer func() { testHookExactCancelPoll = nil }()

	cands, _, stats, err := p.ExecuteStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("context error = %v, want canceled", ctx.Err())
	}
	if cands != nil {
		t.Fatalf("canceled execution returned %d partial candidates", len(cands))
	}
	if stats != (FilterStats{}) {
		t.Fatalf("canceled execution leaked partial stats: %+v", stats)
	}
	if len(polls) != 1 || polls[0] != exactFilteredCancelCheckEvery {
		t.Fatalf("polls = %v, want [%d]", polls, exactFilteredCancelCheckEvery)
	}
}

// The sleep-free hook makes cancellation land exactly at the first filtered
// poll. These fixtures cover every exact-run storage/fallback seam where a
// partial answer or partial execution evidence could otherwise escape.
func TestFilteredExactCancellationDeterministicAcrossSeams(t *testing.T) {
	t.Run("base", func(t *testing.T) {
		l := exactCancelList(t, "")
		entries := make([]Entry, 1500)
		for i := range entries {
			entries[i] = exactCancelEntry(fmt.Sprintf("b%04d", i), "hot key", "OTHER")
		}
		if err := l.Replace(entries); err != nil {
			t.Fatal(err)
		}
		cancelFilteredAtFirstExactPoll(t, l, "hot key")
	})

	t.Run("overlay", func(t *testing.T) {
		l := exactCancelList(t, "")
		if err := l.Replace([]Entry{exactCancelEntry("base", "cold key", "OTHER")}); err != nil {
			t.Fatal(err)
		}
		entries := make([]Entry, 1500)
		for i := range entries {
			entries[i] = exactCancelEntry(fmt.Sprintf("o%04d", i), "hot key", "OTHER")
		}
		if err := l.Upsert(entries); err != nil {
			t.Fatal(err)
		}
		cancelFilteredAtFirstExactPoll(t, l, "hot key")
	})

	t.Run("tombstones", func(t *testing.T) {
		l := exactCancelList(t, "")
		entries := make([]Entry, 1500)
		for i := range entries {
			entries[i] = exactCancelEntry(fmt.Sprintf("t%04d", i), "hot key", "OTHER")
		}
		if err := l.Replace(entries); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 700; i++ {
			if err := l.Delete(fmt.Sprintf("t%04d", i)); err != nil {
				t.Fatal(err)
			}
		}
		cancelFilteredAtFirstExactPoll(t, l, "hot key")
	})

	t.Run("parent fallback", func(t *testing.T) {
		l := exactCancelList(t, "parent_domain")
		entries := make([]Entry, 1500)
		for i := range entries {
			entries[i] = exactCancelEntry(fmt.Sprintf("p%04d", i), "example.com", "OTHER")
		}
		if err := l.Replace(entries); err != nil {
			t.Fatal(err)
		}
		cancelFilteredAtFirstExactPoll(t, l, "mail.example.com")
	})

	t.Run("after base contribution", func(t *testing.T) {
		l := exactCancelList(t, "")
		if err := l.Replace([]Entry{exactCancelEntry("winner", "hot key", "SDN")}); err != nil {
			t.Fatal(err)
		}
		entries := make([]Entry, 1500)
		for i := range entries {
			entries[i] = exactCancelEntry(fmt.Sprintf("o%04d", i), "hot key", "OTHER")
		}
		if err := l.Upsert(entries); err != nil {
			t.Fatal(err)
		}
		cancelFilteredAtFirstExactPoll(t, l, "hot key")
	})
}

// Unfiltered execution intentionally retains the pre-v0.5.1 4,096-ordinal
// cadence. This pins Claudio's scope precision and catches an accidental
// global interval change.
func TestUnfilteredExactCancellationCadenceUnchanged(t *testing.T) {
	l := exactCancelList(t, "")
	entries := make([]Entry, 5000)
	for i := range entries {
		entries[i] = exactCancelEntry(fmt.Sprintf("u%04d", i), "hot key", "OTHER")
	}
	if err := l.Replace(entries); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var polls []int
	testHookExactCancelPoll = func(walked, every int) {
		polls = append(polls, walked)
		if every != exactCancelCheckEvery {
			t.Fatalf("unfiltered exact poll interval = %d, want %d", every, exactCancelCheckEvery)
		}
		cancel()
	}
	defer func() { testHookExactCancelPoll = nil }()

	if cands := l.QueryCtx(ctx, "hot key", QueryOpts{TopK: -1}); cands != nil {
		t.Fatalf("canceled unfiltered execution returned %d candidates", len(cands))
	}
	if len(polls) != 1 || polls[0] != exactCancelCheckEvery {
		t.Fatalf("polls = %v, want [%d]", polls, exactCancelCheckEvery)
	}
}
