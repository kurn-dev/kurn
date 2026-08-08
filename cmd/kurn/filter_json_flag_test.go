package main

// The typed CLI flag is a thin client of the engine's shared parser: no type
// guessing, no second grammar, and no store open after a syntactic failure.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestFilterJSONRejectsConflictAndInvalidBeforeOpen(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "must-not-open")
	err := cmdQuery([]string{"-data", missing, "-list", "people", "-q", "x",
		"-filter", "program=SDN", "-filter-json", `{"program":"SDN"}`})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("filter flag conflict: %v", err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("conflicting flags opened the store: %v", statErr)
	}

	err = cmdQuery([]string{"-data", missing, "-list", "people", "-q", "x",
		"-filter-json", `{"program":{"in":[]}}`})
	if err == nil || !strings.Contains(err.Error(), `filter value for "program"`) {
		t.Fatalf("invalid typed filter: %v", err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("invalid filter opened the store: %v", statErr)
	}
}

func TestFilterJSONTypedExecutionAndChannels(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("people", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "ngram"},
		Filterable: []engine.FilterField{
			{Name: "active", Path: "active"},
			{Name: "risk", Path: "risk"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("people", []engine.Entry{
		{ID: "p1", Keys: []string{"dana kovak"}, Payload: json.RawMessage(`{"active":true,"risk":1.00}`)},
		{ID: "p2", Keys: []string{"dana kovak"}, Payload: json.RawMessage(`{"active":false,"risk":2}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	capture := func(f **os.File) func() string {
		orig := *f
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		*f = w
		return func() string {
			_ = w.Close()
			*f = orig
			b, _ := io.ReadAll(r)
			return string(b)
		}
	}
	outDone := capture(&os.Stdout)
	errDone := capture(&os.Stderr)
	qerr := cmdQuery([]string{"-data", dir, "-list", "people", "-q", "dana kovak",
		"-filter-json", `{"risk":{"in":[2,1e0]},"active":true}`, "-stats"})
	stdout, stderr := outDone(), errDone()
	if qerr != nil {
		t.Fatalf("typed query: %v", qerr)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 || !strings.Contains(stdout, `"p1"`) || strings.Contains(stdout, `"p2"`) {
		t.Fatalf("typed candidate stream: %s", stdout)
	}
	var stats struct {
		FilterStats map[string]struct {
			Evaluated int64 `json:"evaluated"`
			Rejected  int64 `json:"rejected"`
		} `json:"filter_stats"`
	}
	if err := json.Unmarshal([]byte(stderr), &stats); err != nil {
		t.Fatalf("typed stats: %v: %s", err, stderr)
	}
	if got := stats.FilterStats["people"]; got.Evaluated != 2 || got.Rejected != 1 {
		t.Fatalf("typed stats: %+v", got)
	}
}

func TestFilterJSONEmptyIdentityIsUnfiltered(t *testing.T) {
	err := cmdQuery([]string{"-data", t.TempDir(), "-list", "people", "-q", "x",
		"-filter-json", `{}`, "-stats"})
	if err == nil || !strings.Contains(err.Error(), "requires a non-empty") {
		t.Fatalf("empty typed stats misuse: %v", err)
	}
}
