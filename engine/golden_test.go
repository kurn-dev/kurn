package engine_test

// Golden probes live in list config — validated at creation,
// persisted in config.json, surfaced back via Config().

import (
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestGoldenProbeValidation(t *testing.T) {
	base := exactCfg()
	for _, tc := range []struct {
		name  string
		probe engine.GoldenProbe
		ok    bool
	}{
		{"expect-id", engine.GoldenProbe{Q: "aa-1", ExpectID: "c1"}, true},
		{"expect-id-with-score", engine.GoldenProbe{Q: "aa-1", ExpectID: "c1", MinScore: 90}, true},
		{"absent", engine.GoldenProbe{Q: "no-such", Absent: true}, true},
		{"empty-q", engine.GoldenProbe{ExpectID: "c1"}, false},
		{"neither", engine.GoldenProbe{Q: "aa-1"}, false},
		{"both", engine.GoldenProbe{Q: "aa-1", ExpectID: "c1", Absent: true}, false},
		{"score-with-absent", engine.GoldenProbe{Q: "x", Absent: true, MinScore: 50}, false},
		{"score-out-of-range", engine.GoldenProbe{Q: "aa-1", ExpectID: "c1", MinScore: 101}, false},
		{"negative-score", engine.GoldenProbe{Q: "aa-1", ExpectID: "c1", MinScore: -1}, false},
	} {
		cfg := base
		cfg.Golden = []engine.GoldenProbe{tc.probe}
		_, err := engine.NewList("codes", cfg)
		if tc.ok && err != nil {
			t.Errorf("%s: rejected valid probe: %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: accepted invalid probe", tc.name)
		}
	}
}

func TestGoldenConfigRoundTrips(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := exactCfg()
	cfg.Golden = []engine.GoldenProbe{
		{Q: "AA-1", ExpectID: "c1", MinScore: 100},
		{Q: "definitely-absent", Absent: true},
	}
	if _, err := st.CreateList("codes", cfg); err != nil {
		t.Fatal(err)
	}
	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := st2.List("codes")
	if !ok {
		t.Fatal("list missing")
	}
	g := l.Config().Golden
	if len(g) != 2 || g[0].ExpectID != "c1" || g[0].MinScore != 100 || !g[1].Absent {
		t.Fatalf("golden config did not round-trip: %+v", g)
	}
}
