package engine_test

// Iteration-5 charge-model evidence: filtered vs unfiltered cost across
// payload size × path count × hit/miss × floor shape. Not a CI gate — the
// numbers feed the charge constants' comment and the implementation note.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func benchPayload(size int) string {
	// program plus filler to reach ~size bytes
	base := `{"program":"SDN","country":"RU","type":"Individual","listed":"2024-01-15","meta":{"program":"SDN","source":"ofac"},"k3":"x","k4":"y","k5":"z","k6":"w","k7":"v","filler":"`
	return base + strings.Repeat("a", size) + `"}`
}

func buildBenchList(b *testing.B, n int, payloadSize int) *engine.List {
	b.Helper()
	cfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "ngram"},
		Filterable: []engine.FilterField{
			{Name: "program", Path: "program"},
			{Name: "country", Path: "country"},
			{Name: "type", Path: "type"},
			{Name: "source", Path: "meta.source"},
			{Name: "k3", Path: "k3"},
			{Name: "k4", Path: "k4"},
			{Name: "k5", Path: "k5"},
			{Name: "k6", Path: "k6"},
		},
	}
	l, err := engine.NewList("bench", cfg)
	if err != nil {
		b.Fatal(err)
	}
	p := benchPayload(payloadSize)
	entries := make([]engine.Entry, n)
	for i := range entries {
		prog := "OTHER"
		if i == n-42 { // one late hit
			prog = "SDN"
		}
		entries[i] = engine.Entry{
			ID:      fmt.Sprintf("p%d", i),
			Keys:    []string{fmt.Sprintf("dana kovak %d", i%97)},
			Payload: []byte(strings.Replace(p, `"program":"SDN"`, `"program":"`+prog+`"`, 1)),
		}
	}
	if err := l.Replace(entries); err != nil {
		b.Fatal(err)
	}
	return l
}

func BenchmarkFilterMatrix(b *testing.B) {
	small := buildBenchList(b, 20000, 60)
	large := buildBenchList(b, 20000, 1500)
	lists := map[string]*engine.List{"small": small, "large": large}
	// The single SDN payload sits at index n-42 → key "dana kovak 73".
	hitKey := "dana kovak 73"
	onePath := map[string]string{"program": "SDN"}
	maxPaths := map[string]string{
		"program": "SDN", "country": "RU", "type": "Individual", "source": "ofac",
		"k3": "x", "k4": "y", "k5": "z", "k6": "w",
	}
	noneMatch := func(in map[string]string) map[string]string {
		out := make(map[string]string, len(in))
		for k := range in {
			out[k] = "ZZZ-NO-MATCH"
		}
		return out
	}
	ctx := context.Background()
	for _, lname := range []string{"small", "large"} {
		l := lists[lname]
		for _, pname := range []string{"p1", "p8"} {
			filter := onePath
			if pname == "p8" {
				filter = maxPaths
			}
			for _, outcome := range []string{"hit", "miss"} {
				q := hitKey // hit: reaches the one SDN payload
				if outcome == "miss" {
					filter = noneMatch(filter) // no payload carries these values
				}
				for _, floor := range []string{"floor", "flood"} {
					opts := engine.QueryOpts{}
					if floor == "flood" {
						opts.Threshold = -1
					}
					name := fmt.Sprintf("%s/%s/%s/%s", lname, pname, outcome, floor)
					// Outcome honesty: prove the intended hit/miss shape
					// actually occurred before measuring.
					var probe []engine.Candidate
					var perr error
					probe, _, perr = l.QueryFilteredCtx(ctx, q, opts, filter)
					if perr != nil {
						b.Fatal(perr)
					}
					if outcome == "hit" && len(probe) == 0 {
						b.Fatalf("%s: fixture intended a hit, got none", name)
					}
					if outcome == "miss" && len(probe) != 0 {
						b.Fatalf("%s: fixture intended a miss, got %d", name, len(probe))
					}
					b.Run(name+"/filtered", func(b *testing.B) {
						b.ReportAllocs()
						for i := 0; i < b.N; i++ {
							if _, _, err := l.QueryFilteredCtx(ctx, q, opts, filter); err != nil {
								b.Fatal(err)
							}
						}
					})
					b.Run(name+"/unfiltered", func(b *testing.B) {
						b.ReportAllocs()
						for i := 0; i < b.N; i++ {
							l.QueryCtx(ctx, q, opts)
						}
					})
				}
			}
		}
	}
}
