package main

// The -stats flag contract: refused without -filter (a silent no-op
// would read as "zero candidates evaluated"), stdout stays a pure
// candidate-per-line stream, and the single stats object lands on
// stderr mirroring the HTTP filter_stats member shape.

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestStatsFlagRequiresFilter(t *testing.T) {
	err := cmdQuery([]string{"-data", t.TempDir(), "-list", "people", "-q", "x", "-stats"})
	if err == nil || !strings.Contains(err.Error(), "-stats requires -filter") {
		t.Fatalf("want the -stats-requires-filter error, got %v", err)
	}
}

func TestStatsFlagOutputChannels(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("people", engine.ListConfig{
		Analyzer:   engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:      engine.MatchConfig{Mode: "ngram"},
		Filterable: []engine.FilterField{{Name: "program", Path: "program"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("people", []engine.Entry{
		{ID: "p1", Keys: []string{"dana kovak"}, Payload: json.RawMessage(`{"program":"SDN"}`)},
		{ID: "p2", Keys: []string{"dana kovak"}, Payload: json.RawMessage(`{"program":"EU"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	capture := func(f **os.File) (restore func() string) {
		orig := *f
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		*f = w
		return func() string {
			w.Close()
			*f = orig
			b, _ := io.ReadAll(r)
			return string(b)
		}
	}
	outDone := capture(&os.Stdout)
	errDone := capture(&os.Stderr)
	qerr := cmdQuery([]string{"-data", dir, "-list", "people", "-q", "dana kovak",
		"-filter", "program=SDN", "-stats"})
	stdout, stderr := outDone(), errDone()
	if qerr != nil {
		t.Fatalf("query failed: %v\nstderr: %s", qerr, stderr)
	}

	// stdout: candidate lines only — every line is a candidate object,
	// none mentions filter_stats.
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"p1"`) || strings.Contains(stdout, "filter_stats") {
		t.Fatalf("stdout is not a pure candidate stream:\n%s", stdout)
	}
	// stderr: exactly one JSON object, the HTTP member shape keyed by list.
	var stats struct {
		FilterStats map[string]struct {
			Evaluated int64 `json:"evaluated"`
			Rejected  int64 `json:"rejected"`
		} `json:"filter_stats"`
	}
	if err := json.Unmarshal([]byte(stderr), &stats); err != nil {
		t.Fatalf("stderr is not one JSON object: %v\n%s", err, stderr)
	}
	fs, ok := stats.FilterStats["people"]
	if !ok || fs.Evaluated != 2 || fs.Rejected != 1 {
		t.Fatalf("stats = %+v ok=%v, want people evaluated=2 rejected=1", fs, ok)
	}

	// Without -stats: stderr stays empty (the default channels are
	// unchanged — the flag is the only way to get the object).
	outDone = capture(&os.Stdout)
	errDone = capture(&os.Stderr)
	qerr = cmdQuery([]string{"-data", dir, "-list", "people", "-q", "dana kovak",
		"-filter", "program=SDN"})
	stdout, stderr = outDone(), errDone()
	if qerr != nil {
		t.Fatalf("query failed: %v", qerr)
	}
	if stderr != "" {
		t.Fatalf("stderr must stay empty without -stats: %s", stderr)
	}
	if !strings.Contains(stdout, `"p1"`) {
		t.Fatalf("candidate stream changed: %s", stdout)
	}
}
