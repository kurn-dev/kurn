package engine_test

// Filterable declarations: validation, normalization, clone safety, and
// the digest/stamp identity contract (a declaration change is an identity
// change; declaration ORDER is not).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kurn-dev/kurn/engine"
)

func TestFilterableValidation(t *testing.T) {
	base := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase"}},
		Match:    engine.MatchConfig{Mode: "ngram"},
	}
	good := []engine.FilterField{{Name: "program", Path: "program"}}
	for name, fields := range map[string][]engine.FilterField{
		"empty name":      {{Name: "", Path: "x"}},
		"empty path":      {{Name: "x", Path: ""}},
		"duplicate names": {{Name: "a", Path: "x"}, {Name: "a", Path: "y"}},
		"overlong name":   {{Name: strings.Repeat("n", 129), Path: "x"}},
		"overlong path":   {{Name: "x", Path: strings.Repeat("p", 257)}},
		"nine fields":     {{Name: "1", Path: "a"}, {Name: "2", Path: "a"}, {Name: "3", Path: "a"}, {Name: "4", Path: "a"}, {Name: "5", Path: "a"}, {Name: "6", Path: "a"}, {Name: "7", Path: "a"}, {Name: "8", Path: "a"}, {Name: "9", Path: "a"}},
	} {
		cfg := base
		cfg.Filterable = fields
		if _, err := engine.NewList("bad", cfg); err == nil {
			t.Errorf("%s: accepted invalid filterable", name)
		}
	}
	cfg := base
	cfg.Filterable = good
	if _, err := engine.NewList("good", cfg); err != nil {
		t.Fatalf("valid declaration refused: %v", err)
	}
}

func TestFilterableNormalizationAndClone(t *testing.T) {
	mk := func(order []string) engine.ListConfig {
		var f []engine.FilterField
		for _, n := range order {
			f = append(f, engine.FilterField{Name: n, Path: "p." + n})
		}
		return engine.ListConfig{
			Analyzer:   engine.AnalyzerConfig{Steps: []string{"lowercase"}},
			Match:      engine.MatchConfig{Mode: "ngram"},
			Filterable: f,
		}
	}
	a, err := engine.NewList("a", mk([]string{"zeta", "alpha"}))
	if err != nil {
		t.Fatal(err)
	}
	b, err := engine.NewList("b", mk([]string{"alpha", "zeta"}))
	if err != nil {
		t.Fatal(err)
	}
	if a.ConfigDigest() != b.ConfigDigest() {
		t.Fatal("declaration ORDER moved the digest — normalization broken")
	}
	if got := a.Config().Filterable[0].Name; got != "alpha" {
		t.Fatalf("config not name-sorted: %q first", got)
	}
	// Clone safety: a caller mutating its own slice must not reach the
	// list's validated, normalized config.
	own := mk([]string{"alpha"})
	l, err := engine.NewList("c", own)
	if err != nil {
		t.Fatal(err)
	}
	own.Filterable[0].Path = "hacked"
	if l.Config().Filterable[0].Path == "hacked" {
		t.Fatal("caller slice mutation reached the list's config")
	}
}

// Adding a declaration to an existing list moves the stamp's config half
// but not its base half, and must not force a rebuild: the artifact's
// identity checks (base hash, analyzer, grams) are untouched by it, so the
// artifact file must not be rewritten on reopen.
func TestFilterableDeclarationIsStampOnly(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "ngram"},
	}
	if _, err := st.CreateList("people", cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("people", []engine.Entry{
		{ID: "p1", Keys: []string{"Marcus Chen"}, Payload: json.RawMessage(`{"program":"SDN"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	l, _ := st.List("people")
	v1 := l.Version()
	idxPath := filepath.Join(dir, "people", "base.idx")
	fi1, err := os.Stat(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	// An operator (or future migration) edits config.json: add a
	// declaration. Base bytes and artifact stay as they were.
	cfgPath := filepath.Join(dir, "people", "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk engine.ListConfig
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	onDisk.Filterable = []engine.FilterField{{Name: "program", Path: "program"}}
	out, err := json.Marshal(onDisk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	l2, ok := st2.List("people")
	if !ok {
		t.Fatal("list skipped after declaration-only config edit")
	}
	v2 := l2.Version()
	baseHalf := func(v string) string { return strings.Split(v, "+c")[0] }
	if baseHalf(v1) != baseHalf(v2) {
		t.Fatalf("base identity moved on a declaration change: %s vs %s", v1, v2)
	}
	if v1 == v2 {
		t.Fatal("declaration change did not move the version stamp")
	}
	fi2, err := os.Stat(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi2.ModTime() != fi1.ModTime() || fi2.Size() != fi1.Size() {
		t.Fatal("artifact rewritten on a declaration-only change — a rebuild fired")
	}
	if c := l2.Query("marcus chen", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "p1" {
		t.Fatalf("list does not serve after declaration change: %+v", c)
	}
	time.Sleep(time.Millisecond) // let any async writer surface before Close
}
