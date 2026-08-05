package engine_test

// Regression tests: Artifacts now record the analyzer
// spec digest, so an analyzer change between restarts (a hand-edited
// config.json) can no longer silently install a stale index — queries
// normalized with the new analyzer against keys normalized with the old.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/artifact"
	"github.com/kurn-dev/kurn/engine/exact"
)

func punctCfg() engine.ListConfig {
	return engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "strip_punctuation"}},
		Match:    engine.MatchConfig{Mode: "exact"},
	}
}

// Hand-editing the analyzer steps in config.json between restarts must force
// a full rebuild: the artifact's keys were normalized with the OLD analyzer
// and are unusable. Discriminator: with strip_punctuation, key "A.A"
// analyzes to "aa"; without it, to "a.a". If the stale artifact were
// installed, queries (normalized with the NEW analyzer) would hit on "aa"
// and miss on "a.a" — the rebuild must produce exactly the reverse.
func TestAnalyzerChangeForcesRebuild(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("codes", punctCfg()); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("codes", []engine.Entry{{ID: "e1", Keys: []string{"A.A"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "codes", "base.idx")); err != nil {
		t.Fatalf("no artifact saved: %v", err)
	}

	// Hand-edit config.json: drop strip_punctuation from the analyzer steps.
	cfgPath := filepath.Join(dir, "codes", "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), `,"strip_punctuation"`, "", 1)
	if edited == string(raw) {
		t.Fatalf("config edit did not apply: %s", raw)
	}
	if err := os.WriteFile(cfgPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := st2.List("codes")
	if !ok {
		t.Fatal("list missing after config edit")
	}
	if c := l.Query("a.a", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "e1" {
		t.Fatalf("rebuild with new analyzer did not happen (stale artifact installed?): %+v", c)
	}
	if c := l.Query("aa", engine.QueryOpts{}); len(c) != 0 {
		t.Fatalf("stale index keys still live after analyzer change: %+v", c)
	}
}

// Step ARGUMENTS contain commas (strip_words lists), so the digest must not
// be a comma-join: ["strip_words:mr,trim"] and ["strip_words:mr","trim"] are
// different analyzers and must have different digests — a collision here
// silently installs a stale index, the exact failure the digest prevents.
func TestAnalyzerSpecDigestUnambiguous(t *testing.T) {
	a1, err := engine.ResolveAnalyzer(engine.AnalyzerConfig{Steps: []string{"strip_words:mr,trim"}})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := engine.ResolveAnalyzer(engine.AnalyzerConfig{Steps: []string{"strip_words:mr", "trim"}})
	if err != nil {
		t.Fatal(err)
	}
	if d1, d2 := engine.AnalyzerSpecDigest(a1), engine.AnalyzerSpecDigest(a2); d1 == d2 {
		t.Fatalf("distinct analyzer specs share digest %q", d1)
	}
	// Same spec ⇒ same digest (stability).
	a3, err := engine.ResolveAnalyzer(engine.AnalyzerConfig{Steps: []string{"strip_words:mr,trim"}})
	if err != nil {
		t.Fatal(err)
	}
	if d1, d3 := engine.AnalyzerSpecDigest(a1), engine.AnalyzerSpecDigest(a3); d1 != d3 {
		t.Fatalf("identical specs digest differently: %q vs %q", d1, d3)
	}
}

// An artifact written before the digest field existed (or with "" digest)
// records an UNKNOWN analyzer — the install path must reject it and rebuild
// rather than trust it.
func TestPreDigestArtifactRebuilds(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("codes", exactCfg()); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("codes", []engine.Entry{{ID: "c1", Keys: []string{"AA-1"}}}); err != nil {
		t.Fatal(err)
	}

	// Overwrite base.idx with a valid index carrying no analyzer digest,
	// built from a different key — pre-fix this would have been installed.
	b := exact.NewBuilder()
	b.Add(0, []string{"doctored-key"})
	doctored, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if err := artifact.SaveExact(filepath.Join(dir, "codes", "base.idx"), doctored, "", artifact.BuildInfo{}); err != nil {
		t.Fatal(err)
	}

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := st2.List("codes")
	if c := l.Query("doctored-key", engine.QueryOpts{}); len(c) != 0 {
		t.Fatalf("digest-less artifact installed: %+v", c)
	}
	if c := l.Query("aa-1", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "c1" {
		t.Fatalf("fallback rebuild wrong: %+v", c)
	}
}
