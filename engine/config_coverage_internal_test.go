package engine

// White-box companion to TestConfigDigestFieldCoverage: golden.absent is
// mutually exclusive with expect_id in a VALID probe, so the public-API
// mutation table can only flip probe kind — two leaves at once, which
// would still pass if Absent itself escaped the digest. This probe calls
// the private hash primitive directly on two synthetic configs differing
// ONLY in Absent. Bypassing NewList validation is deliberate: the subject
// is the hash's field coverage, not config validity.

import "testing"

func TestConfigDigestAbsentLeafIsolated(t *testing.T) {
	cfg := ListConfig{
		Analyzer: AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    MatchConfig{Mode: "ngram"},
		Golden:   []GoldenProbe{{Q: "alpha", ExpectID: "e1"}},
	}
	an, err := ResolveAnalyzer(cfg.Analyzer)
	if err != nil {
		t.Fatal(err)
	}
	before := resolvedConfigDigest(cfg, an)
	cfg2 := cfg
	cfg2.Golden = []GoldenProbe{{Q: "alpha", ExpectID: "e1", Absent: true}}
	if after := resolvedConfigDigest(cfg2, an); after == before {
		t.Fatal("flipping Absent alone did not move the digest — the leaf escapes the hash")
	}
}
