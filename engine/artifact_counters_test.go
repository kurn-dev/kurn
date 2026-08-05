package engine_test

// Build-loss counters across the artifact fast path. A reopen that loads
// base.idx skips analysis, so it cannot recompute what analysis lost; the
// counters therefore travel inside the artifact. Before that, a normal
// restart silently reset them — and for ngram it went further and
// reattributed the loss to a stale artifact, naming a repair (rebuild the
// bundle) for a condition whose real repair is the analyzer.

import (
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

// The counter still has to fire for the condition it names. Truncating
// base.jsonl's entry list while keeping the artifact leaves a list whose
// index genuinely does not cover every entry.
func TestStaleArtifactIsReportedAsUnindexed(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := lossyList("ngram")
	if _, err := st.CreateList("l", cfg); err != nil {
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

	// Append an entry to base.jsonl that the saved artifact never saw —
	// the shape a crash between writing the two files leaves behind.
	bp := filepath.Join(dir, "l", "base.jsonl")
	raw, err := os.ReadFile(bp)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte(`{"id":"c","keys":["charlie"]}`+"\n")...)
	if err := os.WriteFile(bp, raw, 0o644); err != nil {
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
	if n, _, _ := l2.Stats(); n != 3 {
		t.Fatalf("entries = %d, want 3", n)
	}
	if u := l2.UnindexedEntries(); u != 1 {
		t.Fatalf("unindexed = %d, want 1 — the appended entry is unreachable", u)
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
