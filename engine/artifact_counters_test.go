package engine_test

// Build-loss counters across the artifact fast path. A reopen that loads
// base.idx skips analysis, so it cannot recompute what analysis lost; the
// counters therefore travel inside the artifact. Before that, a normal
// restart silently reset them — and for ngram it went further and
// reattributed the loss to a stale artifact, naming a repair (rebuild the
// bundle) for a condition whose real repair is the analyzer.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/artifact"
)

func lossyList(mode string) engine.ListConfig {
	return engine.ListConfig{
		// strip_punctuation + trim collapse punctuation-only keys to "".
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "strip_punctuation", "trim"}},
		Match:    engine.MatchConfig{Mode: mode},
	}
}

func TestLossCountersSurviveArtifactReload(t *testing.T) {
	for _, mode := range []string{"ngram", "exact"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			st, err := engine.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateList("l", lossyList(mode)); err != nil {
				t.Fatal(err)
			}
			if err := st.Replace("l", []engine.Entry{
				{ID: "a", Keys: []string{"fine", "..."}}, // one key dropped
				{ID: "b", Keys: []string{"!!!"}},         // every key dropped: keyless
			}); err != nil {
				t.Fatal(err)
			}
			l, _ := st.List("l")
			wantD, wantK := l.BuildStats()
			if wantD != 2 || wantK != 1 {
				t.Fatalf("setup: dropped=%d keyless=%d, want 2 and 1", wantD, wantK)
			}
			if u := l.UnindexedEntries(); u != 0 {
				t.Fatalf("setup: unindexed=%d, want 0", u)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}

			// Reopen: the artifact fast path must be the one that runs, or
			// this proves nothing about it.
			if _, err := os.Stat(filepath.Join(dir, "l", "base.idx")); err != nil {
				t.Fatalf("no artifact to reload: %v", err)
			}
			st2, err := engine.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			l2, ok := st2.List("l")
			if !ok {
				t.Fatal("list lost on reopen")
			}
			gotD, gotK := l2.BuildStats()
			if gotD != wantD || gotK != wantK {
				t.Errorf("after reopen: dropped=%d keyless=%d, want %d and %d — the counters reset",
					gotD, gotK, wantD, wantK)
			}
			// And the keyless entry must NOT be reattributed to a stale
			// artifact: the artifact is current, it simply indexes nothing
			// for that entry.
			if u := l2.UnindexedEntries(); u != 0 {
				t.Errorf("after reopen: unindexed=%d, want 0 — analyzer loss was blamed on a stale artifact", u)
			}
		})
	}
}

// A base.jsonl that no longer matches the artifact must REBUILD, not
// install. The first version of this test expected the mismatch to install
// with unindexed_entries counting the gap — but an index maps grams to
// ordinal positions in one specific base content, and against modified
// content of the same length every count can agree while queries attribute
// one entry's evidence to another (the second follow-up review demonstrated
// entity C returned with entity A's score and key). Identity, not counts,
// is what the install path checks now; the unindexed counter remains for
// library callers who install a deliberately partial index WITH its info.
func TestModifiedBaseRebuildsInsteadOfServingTheArtifact(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func([]byte) []byte
	}{
		{"append", func(raw []byte) []byte {
			return append(raw, []byte(`{"id":"c","keys":["charlie"]}`+"\n")...)
		}},
		{"same-length swap", func(raw []byte) []byte {
			return bytes.ReplaceAll(raw, []byte("alpha"), []byte("gamma"))
		}},
		{"reorder", func(raw []byte) []byte {
			lines := bytes.SplitAfter(raw, []byte("\n"))
			lines[0], lines[1] = lines[1], lines[0]
			return bytes.Join(lines, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st, err := engine.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateList("l", lossyList("ngram")); err != nil {
				t.Fatal(err)
			}
			if err := st.Replace("l", []engine.Entry{
				{ID: "a", Keys: []string{"alpha"}},
				{ID: "b", Keys: []string{"bravo"}},
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}

			bp := filepath.Join(dir, "l", "base.jsonl")
			raw, err := os.ReadFile(bp)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bp, tc.mut(raw), 0o644); err != nil {
				t.Fatal(err)
			}

			st2, err := engine.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			l2, ok := st2.List("l")
			if !ok {
				t.Fatal("list lost on reopen")
			}
			// Rebuilt from the current base: every present key finds its
			// OWN entry, and nothing is reported unreachable.
			if u := l2.UnindexedEntries(); u != 0 {
				t.Fatalf("unindexed = %d, want 0 after a rebuild", u)
			}
			// Assert entity identity, not key-shaped echoes: with stale
			// postings a query returns the WRONG entry whose own key is
			// attributed to it, so a check conditioned on key==query
			// passes vacuously (the first version of this test did).
			mustFind := func(q, want string) {
				t.Helper()
				c := l2.Query(q, engine.QueryOpts{})
				if len(c) == 0 || c[0].EntryID != want {
					t.Fatalf("query %q -> %+v, want entry %s", q, c, want)
				}
			}
			mustMiss := func(q string) {
				t.Helper()
				if c := l2.Query(q, engine.QueryOpts{}); len(c) != 0 {
					t.Fatalf("query %q found %+v, want nothing — a removed key still resolves", q, c)
				}
			}
			switch tc.name {
			case "append":
				mustFind("alpha", "a")
				mustFind("charlie", "c")
			case "same-length swap":
				mustFind("gamma", "a")
				mustFind("bravo", "b")
				mustMiss("alpha")
			case "reorder":
				mustFind("alpha", "a")
				mustFind("bravo", "b")
			}
		})
	}
}

// Exact mode: the same same-length swap must also rebuild, not serve the
// old postings.
func TestModifiedBaseRebuildsExact(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("l", lossyList("exact")); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("l", []engine.Entry{
		{ID: "a", Keys: []string{"alpha"}},
		{ID: "b", Keys: []string{"bravo"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	bp := filepath.Join(dir, "l", "base.jsonl")
	raw, err := os.ReadFile(bp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bp, bytes.ReplaceAll(raw, []byte("alpha"), []byte("gamma")), 0o644); err != nil {
		t.Fatal(err)
	}
	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l2, _ := st2.List("l")
	if c := l2.Query("alpha", engine.QueryOpts{}); len(c) != 0 {
		t.Fatalf("stale postings served: %+v", c)
	}
	if c := l2.Query("gamma", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "a" {
		t.Fatalf("rebuilt index wrong: %+v", c)
	}
}

// An artifact written before the build record existed carries no counters,
// and there is no way to recover them without re-analyzing. Rebuild once
// rather than serve numbers that were never measured — the same escape
// hatch a pre-digest artifact takes.
func TestArtifactWithoutBuildRecordRebuilds(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("l", lossyList("ngram")); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("l", []engine.Entry{
		{ID: "a", Keys: []string{"fine", "..."}},
		{ID: "b", Keys: []string{"!!!"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-save the same index with no build record — what an older release
	// wrote, and what the loader must treat as unknown rather than zero.
	ip := filepath.Join(dir, "l", "base.idx")
	idx, digest, _, err := artifact.Load(ip)
	if err != nil {
		t.Fatal(err)
	}
	if err := artifact.Save(ip, idx, digest, artifact.BuildInfo{}); err != nil {
		t.Fatal(err)
	}

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l2, ok := st2.List("l")
	if !ok {
		t.Fatal("list lost on reopen")
	}
	// Rebuilt from base.jsonl, so the counters are measured again rather
	// than guessed, and nothing is blamed on a stale artifact.
	d, k := l2.BuildStats()
	if d != 2 || k != 1 {
		t.Fatalf("after rebuild: dropped=%d keyless=%d, want 2 and 1", d, k)
	}
	if u := l2.UnindexedEntries(); u != 0 {
		t.Fatalf("after rebuild: unindexed=%d, want 0", u)
	}
}
