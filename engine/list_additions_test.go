package engine_test

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/exact"
	"github.com/kurn-dev/kurn/engine/ngram"
)

func buildIdx(t *testing.T, keys ...string) *ngram.Index {
	t.Helper()
	b := ngram.NewBuilder(ngram.Config{Grams: []int{2, 3}, StripSpaces: true})
	for i, k := range keys {
		b.Add(uint32(i), []string{k})
	}
	return b.Finish()
}

func TestReplaceWithIndex(t *testing.T) {
	mk := func() *engine.List { return personList(t) }

	t.Run("installs prebuilt index", func(t *testing.T) {
		l := mk()
		idx := buildIdx(t, "marcus chen", "dana kovak")
		entries := []engine.Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}, {ID: "p2", Keys: []string{"Dana Kovak"}}}
		if err := l.ReplaceWithIndex(entries, idx); err != nil {
			t.Fatal(err)
		}
		if c := l.Query("dana kovak", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "p2" {
			t.Fatalf("query via prebuilt index: %+v", c)
		}
		if l.BaseNgram() != idx {
			t.Fatal("BaseNgram is not the installed index")
		}
	})

	t.Run("NumOrds beyond entries rejected", func(t *testing.T) {
		l := mk()
		idx := buildIdx(t, "marcus chen", "dana kovak") // 2 ords
		err := l.ReplaceWithIndex([]engine.Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}}, idx)
		if err == nil {
			t.Fatal("mismatched idx/entries accepted")
		}
		// List must be untouched by the failed install.
		if e, _, _ := l.Stats(); e != 0 {
			t.Fatalf("failed install mutated list: %d entries", e)
		}
	})

	t.Run("dedupe before NumOrds validation", func(t *testing.T) {
		// Two raw entries but a duplicate ID: post-dedupe length is 1, so a
		// 2-ord index must be rejected (the artifact was saved from deduped
		// entries; 2 ords cannot belong to 1 deduped entry).
		l := mk()
		idx := buildIdx(t, "marcus chen", "marcus chao")
		err := l.ReplaceWithIndex([]engine.Entry{
			{ID: "p1", Keys: []string{"Marcus Chen"}},
			{ID: "p1", Keys: []string{"Marcus Chao"}},
		}, idx)
		if err == nil {
			t.Fatal("2-ord index over 1 deduped entry accepted")
		}
	})

	t.Run("exact mode rejected", func(t *testing.T) {
		l, err := engine.NewList("codes", engine.ListConfig{
			Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase"}},
			Match:    engine.MatchConfig{Mode: "exact"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := l.ReplaceWithIndex(nil, buildIdx(t, "x")); err == nil {
			t.Fatal("exact-mode ReplaceWithIndex accepted")
		}
		if l.BaseNgram() != nil {
			t.Fatal("BaseNgram non-nil for exact list")
		}
	})

	t.Run("nil index rejected", func(t *testing.T) {
		l := mk()
		if err := l.ReplaceWithIndex(nil, nil); err == nil {
			t.Fatal("nil index accepted")
		}
	})
}

func buildExIdx(t *testing.T, keys ...string) *exact.Index {
	t.Helper()
	b := exact.NewBuilder()
	for i, k := range keys {
		b.Add(uint32(i), []string{k})
	}
	idx, err := b.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return idx
}

func exactList(t *testing.T) *engine.List {
	t.Helper()
	l, err := engine.NewList("codes", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "exact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestReplaceWithExactIndex(t *testing.T) {
	t.Run("installs prebuilt index", func(t *testing.T) {
		l := exactList(t)
		idx := buildExIdx(t, "aa-1", "bb-2")
		entries := []engine.Entry{{ID: "c1", Keys: []string{"AA-1"}}, {ID: "c2", Keys: []string{"BB-2"}}}
		if err := l.ReplaceWithExactIndex(entries, idx); err != nil {
			t.Fatal(err)
		}
		if c := l.Query("bb-2", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "c2" {
			t.Fatalf("query via prebuilt index: %+v", c)
		}
		if l.BaseExact() != idx {
			t.Fatal("BaseExact is not the installed index")
		}
	})

	t.Run("ordinals beyond entries rejected", func(t *testing.T) {
		l := exactList(t)
		idx := buildExIdx(t, "aa-1", "bb-2") // 2 ords
		err := l.ReplaceWithExactIndex([]engine.Entry{{ID: "c1", Keys: []string{"AA-1"}}}, idx)
		if err == nil {
			t.Fatal("mismatched idx/entries accepted")
		}
		// List must be untouched by the failed install.
		if e, _, _ := l.Stats(); e != 0 {
			t.Fatalf("failed install mutated list: %d entries", e)
		}
	})

	t.Run("dedupe before ordinal validation", func(t *testing.T) {
		// Two raw entries but a duplicate ID: post-dedupe length is 1, so a
		// 2-ord index must be rejected (the artifact was saved from deduped
		// entries; 2 ords cannot belong to 1 deduped entry).
		l := exactList(t)
		idx := buildExIdx(t, "aa-1", "aa-2")
		err := l.ReplaceWithExactIndex([]engine.Entry{
			{ID: "c1", Keys: []string{"AA-1"}},
			{ID: "c1", Keys: []string{"AA-2"}},
		}, idx)
		if err == nil {
			t.Fatal("2-ord index over 1 deduped entry accepted")
		}
	})

	t.Run("ngram mode rejected", func(t *testing.T) {
		l := personList(t)
		if err := l.ReplaceWithExactIndex(nil, buildExIdx(t, "x")); err == nil {
			t.Fatal("ngram-mode ReplaceWithExactIndex accepted")
		}
		if l.BaseExact() != nil {
			t.Fatal("BaseExact non-nil for ngram list")
		}
	})

	t.Run("nil index rejected", func(t *testing.T) {
		l := exactList(t)
		if err := l.ReplaceWithExactIndex(nil, nil); err == nil {
			t.Fatal("nil index accepted")
		}
	})
}

func TestBaseExactEmptyList(t *testing.T) {
	if l := exactList(t); l.BaseExact() != nil {
		t.Fatal("BaseExact non-nil with no base")
	}
}

func TestBaseNgramEmptyList(t *testing.T) {
	l, err := engine.NewList("people", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if l.BaseNgram() != nil {
		t.Fatal("BaseNgram non-nil with no base")
	}
}

func TestLiveEntries(t *testing.T) {
	l := personList(t)
	l.Replace([]engine.Entry{
		{ID: "p2", Keys: []string{"Dana Kovak"}},
		{ID: "p1", Keys: []string{"Marcus Chen"}},
	})
	l.Upsert([]engine.Entry{{ID: "p3", Keys: []string{"Rosa Almeida"}}})
	l.Upsert([]engine.Entry{{ID: "p1", Keys: []string{"Marcus Chao"}}}) // shadows base p1
	l.Delete("p2")

	got := l.LiveEntries()
	if len(got) != 2 || got[0].ID != "p1" || got[1].ID != "p3" {
		t.Fatalf("live entries: %+v", got)
	}
	if got[0].Keys[0] != "Marcus Chao" {
		t.Fatalf("overlay must win over base: %+v", got[0])
	}
}

// A List's validated config must be unreachable through caller-held slices:
// flipping l.Config().Match.Grams[0] to -1 through the shared backing array
// used to panic the next query's gram iteration with index-out-of-range,
// and concurrent mutation was an ordinary data race. Both aliasing
// directions are pinned — the config given to NewList and the view Config
// returns.
func TestConfigIsDetachedFromCallers(t *testing.T) {
	cfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, Threshold: 0.6, TopK: 100},
		Golden:   []engine.GoldenProbe{{Q: "marcus chen", ExpectID: "p1"}},
	}
	l, err := engine.NewList("people", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}}); err != nil {
		t.Fatal(err)
	}
	query := func(when string) {
		t.Helper()
		if c := l.Query("marcus chen", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "p1" {
			t.Fatalf("%s: query broken: %+v", when, c)
		}
	}
	query("baseline")

	// Direction 1: the caller keeps mutating the config it passed in.
	cfg.Match.Grams[0] = -1
	cfg.Analyzer.Steps[0] = "strip_punctuation"
	cfg.Golden[0].Q = "someone else"
	query("after mutating the NewList argument")

	// Direction 2: the caller mutates the view Config returns.
	view := l.Config()
	view.Match.Grams[0] = -1
	view.Analyzer.Steps[0] = "strip_punctuation"
	view.Golden[0].ExpectID = "intruder"
	query("after mutating the Config() view")

	// The list still reports its real configuration.
	got := l.Config()
	if got.Match.Grams[0] != 2 || got.Analyzer.Steps[0] != "lowercase" || got.Golden[0].ExpectID != "p1" {
		t.Fatalf("config leaked caller mutations: %+v", got)
	}
}

// NaN passes every ordinary range comparison, so an explicit rejection is
// the only thing between a NaN threshold and a silently floorless list
// (every downstream score comparison false). Same for golden min_score.
func TestNonFiniteConfigRejected(t *testing.T) {
	nan := math.NaN()
	if _, err := engine.NewList("codes", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase"}},
		Match:    engine.MatchConfig{Mode: "ngram", Threshold: nan},
	}); err == nil {
		t.Error("NaN threshold accepted")
	}
	if _, err := engine.NewList("codes", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase"}},
		Match:    engine.MatchConfig{Mode: "exact"},
		Golden:   []engine.GoldenProbe{{Q: "x", ExpectID: "p1", MinScore: nan}},
	}); err == nil {
		t.Error("NaN golden min_score accepted")
	}
	// Infinities were already caught by the range comparisons; pin that.
	if _, err := engine.NewList("codes", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase"}},
		Match:    engine.MatchConfig{Mode: "ngram", Threshold: math.Inf(1)},
	}); err == nil {
		t.Error("+Inf threshold accepted")
	}
}

// Admission charges a query's cost from one snapshot; execution must run
// THAT snapshot. Pricing the current state, waiting on a budget, and then
// loading a fresh snapshot lets a mutation that landed during the wait
// grow the executed work arbitrarily past the charge. PrepareQuery pins
// snapshot, cost, and execution together — a mutation between prepare and
// execute must change none of them.
func TestPreparedQueryExecutesTheSnapshotItCharged(t *testing.T) {
	l, err := engine.NewList("people", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, Threshold: 0.6, TopK: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}}); err != nil {
		t.Fatal(err)
	}
	vBefore := l.Version()

	p := l.PrepareQuery("marcus chen", engine.QueryOpts{TopK: 5})
	costBefore := p.Cost()

	// The "mutation while queued": replace the list with a much larger
	// corpus whose scratch cost dwarfs the prepared one.
	entries := make([]engine.Entry, 5000)
	for i := range entries {
		entries[i] = engine.Entry{ID: fmt.Sprintf("p%05d", i), Keys: []string{fmt.Sprintf("veko rima %04d", i)}}
	}
	if err := l.Replace(entries); err != nil {
		t.Fatal(err)
	}
	// Validity guard: the mutation really did inflate the current cost.
	if now := l.ScratchBytesFor(5); now <= 4*costBefore {
		t.Fatalf("mutation did not inflate the cost (%d -> %d); the scenario shows nothing", costBefore, now)
	}

	if got := p.Cost(); got != costBefore {
		t.Errorf("prepared cost changed %d -> %d after the mutation", costBefore, got)
	}
	cands, ver := p.Execute(context.Background())
	if ver != vBefore {
		t.Errorf("executed version %q, want the prepared snapshot's %q", ver, vBefore)
	}
	if len(cands) != 1 || cands[0].EntryID != "p1" {
		t.Errorf("executed candidates %+v, want the prepared snapshot's p1", cands)
	}
}
