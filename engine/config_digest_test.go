package engine_test

// ConfigDigest is the public face of the resolved-config identity: equal
// resolved configurations share a digest, any semantic config change moves
// it, and it is exactly the "+c" half of a store-managed version stamp —
// the property bundle manifests embed and loaders verify.

import (
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestConfigDigestIdentity(t *testing.T) {
	cfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, Threshold: 0.6, TopK: 100},
	}
	a, err := engine.NewList("a", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.ConfigDigest()) != 64 {
		t.Fatalf("digest %q is not 64-hex", a.ConfigDigest())
	}
	b, err := engine.NewList("b", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if a.ConfigDigest() != b.ConfigDigest() {
		t.Fatal("equal resolved configs produced different digests")
	}
	cfg2 := cfg
	cfg2.Match.Threshold = 0.5
	c, err := engine.NewList("c", cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if c.ConfigDigest() == a.ConfigDigest() {
		t.Fatal("threshold change did not move the config digest")
	}

	// Store-managed stamps carry the digest verbatim as the +c half.
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l, err := st.CreateList("codes", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(l.Version(), "+c"+l.ConfigDigest()) {
		t.Fatalf("version %q does not end with +c%s", l.Version(), l.ConfigDigest())
	}
}

// The digest of a fully explicit resolved configuration must never drift:
// every persisted bundle, manifest, and platform record in the wild keys
// on these values. Captured from the v0.2.2 implementation; the preset
// normalization (v0.3.0) must not move it, because explicit configs are
// never rewritten.
func TestConfigDigestExplicitGoldenUnchanged(t *testing.T) {
	cfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "fold_diacritics", "strip_punctuation", "strip_words:mr,mrs,ms,dr,prof", "sort_tokens"}},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, Threshold: 0.6, TopK: 100},
	}
	l, err := engine.NewList("golden", cfg)
	if err != nil {
		t.Fatal(err)
	}
	const want = "a44e884c53f256cce53698926d338dfa8a7f1770ced219968985a8fc030f6015"
	if l.ConfigDigest() != want {
		t.Fatalf("explicit-config digest moved: %s, want %s", l.ConfigDigest(), want)
	}
}

// An unknown preset fails outright — normalization must never produce a
// half-resolved list.
func TestConfigDigestUnknownPresetRefused(t *testing.T) {
	cfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "no-such-preset"},
		Match:    engine.MatchConfig{Mode: "ngram"},
	}
	l, err := engine.NewList("bad", cfg)
	if err == nil {
		t.Fatalf("unknown preset produced a list: %+v", l.Config())
	}
}
