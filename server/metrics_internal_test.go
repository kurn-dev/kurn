package server

// White-box: the six per-list gauge families render from one Status()
// capture per list per scrape. Units are captured before rendering, so a
// mutation landing after capture must not leak into any gauge — before the
// one-snapshot conversion, every gauge family reloaded the list
// independently and one scrape could mix generations.

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestListGaugesRenderCapturedSnapshot(t *testing.T) {
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "exact"},
	}
	if _, err := st.CreateList("codes", cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("codes", []engine.Entry{
		{ID: "c1", Keys: []string{"AA-1"}},
		{ID: "c2", Keys: []string{"BB-2"}},
	}); err != nil {
		t.Fatal(err)
	}

	web := NewServer(st, Config{})
	units := web.s.gaugeUnits()
	if len(units) != 1 || units[0].list != "codes" {
		t.Fatalf("units = %+v, want exactly one for codes", units)
	}
	captured := units[0].st

	// Mutate after capture. The guard below proves the mutation moved the
	// gauge-visible fields, so a renderer that reloads cannot pass.
	if err := st.Upsert("codes", []engine.Entry{
		{ID: "c3", Keys: []string{"CC-3"}},
		{ID: "c4", Keys: []string{"DD-4"}},
	}); err != nil {
		t.Fatal(err)
	}
	l, ok := st.List("codes")
	if !ok {
		t.Fatal("list disappeared")
	}
	if live := l.Status(); live.Entries == captured.Entries || live.Overlay == captured.Overlay {
		t.Fatalf("mutation left gauge fields unchanged (live %+v, captured %+v); test is vacuous", live, captured)
	}

	var buf bytes.Buffer
	writeListGauges(&buf, units)
	text := buf.String()
	gauge := func(fam string) int {
		t.Helper()
		for _, line := range strings.Split(text, "\n") {
			if v, ok := strings.CutPrefix(line, fam+`{list="codes"} `); ok {
				n, err := strconv.Atoi(v)
				if err != nil {
					t.Fatalf("parse %q: %v", line, err)
				}
				return n
			}
		}
		t.Fatalf("gauge %s missing:\n%s", fam, text)
		return 0
	}
	for _, g := range []struct {
		fam  string
		want int
	}{
		{"kurn_list_entries", captured.Entries},
		{"kurn_list_overlay", captured.Overlay},
		{"kurn_list_tombstones", captured.Tombstones},
		{"kurn_list_dropped_keys", captured.DroppedKeys},
		{"kurn_list_keyless_entries", captured.KeylessEntries},
		{"kurn_list_unindexed_entries", captured.UnindexedEntries},
	} {
		if v := gauge(g.fam); v != g.want {
			t.Errorf("%s = %d, want captured %d (renderer reloaded the list)", g.fam, v, g.want)
		}
	}
}
