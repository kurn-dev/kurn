package engine_test

// Digest coverage guard: the resolved-config digest is only as strong as
// its claim that EVERY semantic configuration field feeds it. This test
// walks the exported struct graph reachable from ListConfig by reflection
// and requires (a) no reachable json:"-" field, and (b) that the mutation
// table below declares exactly the reflected leaf-path set — so a future
// field addition fails here until its semantic digest case is added.
// It pins coverage, never digest values, and does not make literal
// ordering canonical: the contract intentionally binds the resolved
// representation (see README).

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/analyzer"
)

// leafPaths collects the exported JSON leaf paths of every struct field
// reachable from ListConfig, failing on any field that escapes the digest:
// an unexported field, or one tagged json:"-".
func leafPaths(t *testing.T, typ reflect.Type, prefix string, out map[string]bool) {
	t.Helper()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" {
			t.Fatalf("unexported field %s reachable from ListConfig would escape the digest", f.Name)
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			t.Fatalf(`json:"-" field %s reachable from ListConfig would escape the digest`, f.Name)
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			name = f.Name
		}
		p := name
		if prefix != "" {
			p = prefix + "." + name
		}
		ft := f.Type
		if ft.Kind() == reflect.Slice {
			ft = ft.Elem() // []GoldenProbe walks as golden.*; scalar slices are leaves
		}
		if ft.Kind() == reflect.Struct {
			leafPaths(t, ft, p, out)
		} else {
			out[p] = true
		}
	}
}

func digestOf(t *testing.T, cfg engine.ListConfig) string {
	t.Helper()
	l, err := engine.NewList("coverage", cfg)
	if err != nil {
		t.Fatalf("mutation must stay a valid configuration: %v", err)
	}
	return l.ConfigDigest()
}

func TestConfigDigestFieldCoverage(t *testing.T) {
	ngramBase := func() engine.ListConfig {
		return engine.ListConfig{
			Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
			Match:    engine.MatchConfig{Mode: "ngram"},
		}
	}
	exactBase := func() engine.ListConfig { // fallback is exact-mode only
		c := ngramBase()
		c.Match.Mode = "exact"
		return c
	}
	goldenBase := func() engine.ListConfig {
		c := ngramBase()
		c.Golden = []engine.GoldenProbe{{Q: "alpha", ExpectID: "e1"}}
		return c
	}
	absentBase := func() engine.ListConfig {
		c := ngramBase()
		c.Golden = []engine.GoldenProbe{{Q: "alpha", Absent: true}}
		return c
	}
	presetBase := func() engine.ListConfig {
		c := ngramBase()
		c.Analyzer = engine.AnalyzerConfig{Preset: "person-name"}
		return c
	}

	cases := []struct {
		path string
		base func() engine.ListConfig
		mut  func(*engine.ListConfig)
	}{
		{"match.mode", ngramBase, func(c *engine.ListConfig) { c.Match.Mode = "exact" }},
		{"match.fallback", exactBase, func(c *engine.ListConfig) { c.Match.Fallback = "parent_domain" }},
		{"match.grams", ngramBase, func(c *engine.ListConfig) { c.Match.Grams = []int{2, 3, 4} }},
		{"match.strip_spaces", ngramBase, func(c *engine.ListConfig) { c.Match.StripSpaces = true }},
		{"match.threshold", ngramBase, func(c *engine.ListConfig) { c.Match.Threshold = 0.5 }},
		{"match.topk", ngramBase, func(c *engine.ListConfig) { c.Match.TopK = 50 }},
		// Preset-to-preset, so ONLY the preset leaf moves — a steps→preset
		// mutation changes two leaves and proves neither.
		{"analyzer.preset", presetBase, func(c *engine.ListConfig) {
			c.Analyzer.Preset = "domain"
		}},
		{"analyzer.steps", ngramBase, func(c *engine.ListConfig) {
			c.Analyzer.Steps = []string{"lowercase"}
		}},
		{"golden.q", goldenBase, func(c *engine.ListConfig) { c.Golden[0].Q = "beta" }},
		{"golden.expect_id", goldenBase, func(c *engine.ListConfig) { c.Golden[0].ExpectID = "e2" }},
		{"golden.min_score", goldenBase, func(c *engine.ListConfig) { c.Golden[0].MinScore = 50 }},
		// A valid probe needs exactly one of expect_id/absent, so this
		// public-API case flips probe kind (two leaves at once);
		// TestConfigDigestAbsentLeafIsolated (internal) proves the absent
		// leaf ALONE moves the digest.
		{"golden.absent", absentBase, func(c *engine.ListConfig) {
			c.Golden[0] = engine.GoldenProbe{Q: "alpha", ExpectID: "e1"}
		}},
		{"overlay_auto_compact", ngramBase, func(c *engine.ListConfig) { c.OverlayAutoCompact = 100 }},
	}

	declared := map[string]bool{}
	for _, tc := range cases {
		if declared[tc.path] {
			t.Fatalf("path %s declared twice", tc.path)
		}
		declared[tc.path] = true
		t.Run(strings.ReplaceAll(tc.path, ".", "_"), func(t *testing.T) {
			before := digestOf(t, tc.base())
			mutated := tc.base()
			tc.mut(&mutated)
			if after := digestOf(t, mutated); after == before {
				t.Fatalf("mutating %s did not move the digest", tc.path)
			}
		})
	}

	reflected := map[string]bool{}
	leafPaths(t, reflect.TypeOf(engine.ListConfig{}), "", reflected)
	for p := range reflected {
		if !declared[p] {
			t.Fatalf("config field %s has no digest-mutation case — add one", p)
		}
	}
	for p := range declared {
		if !reflected[p] {
			t.Fatalf("mutation case %s names no reachable config field", p)
		}
	}
}

// A preset and its equivalent expanded steps are the same analyzer, and
// the product path must treat them so: the Store resolves presets to
// explicit steps BEFORE persisting config.json (a future preset redefinition
// must never silently re-analyze an existing list), so two store-managed
// lists configured the two ways share one digest.
//
// Direct NewList calls do not currently satisfy that equality: the digest
// binds the literal ListConfig representation there, preset name included.
// That divergence between the exported documentation and the primitive is
// a known follow-up (direct-NewList normalization, with compatibility
// treatment for already-emitted digests) and is deliberately not pinned
// in either direction here.
func TestConfigDigestPresetResolutionStability(t *testing.T) {
	steps, ok := analyzer.PresetSteps("person-name")
	if !ok {
		t.Fatal("person-name preset unknown")
	}
	presetCfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram"},
	}
	stepsCfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: steps},
		Match:    engine.MatchConfig{Mode: "ngram"},
	}

	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lp, _, err := st.CreateListVersioned("preset", presetCfg)
	if err != nil {
		t.Fatal(err)
	}
	ls, _, err := st.CreateListVersioned("steps", stepsCfg)
	if err != nil {
		t.Fatal(err)
	}
	if lp.ConfigDigest() != ls.ConfigDigest() {
		t.Fatal("store-managed preset and equivalent expanded steps must share a digest")
	}
}
