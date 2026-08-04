package engine_test

import (
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
