package ingest_test

// The T1 accept bar: the shipped declarative mapping
// (docs/examples/ofac.mapping.json) must reproduce the hand-rolled
// bench/ofac extractor on a real SDN publication. Gated on KURN_SDN_XML
// (the feed is not in the repo, per the test-data discipline); run with:
//
//	KURN_SDN_XML=/path/to/sdn.xml go test ./ingest/ -run TestOFACMappingEquivalence
//
// Equivalence is per-entry POST-ANALYSIS key sets — what the index
// actually sees. That absorbs the one intentional delta: bench/ofac
// excludes aliases case-insensitively equal to the primary (5 in the
// 2026-07-29 publication), which the equals-only mapping cannot express;
// those aliases analyze identically to the primary under the person-name
// preset, so the analyzed sets coincide.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/ingest"
)

func TestOFACMappingEquivalence(t *testing.T) {
	sdn := os.Getenv("KURN_SDN_XML")
	if sdn == "" {
		t.Skip("set KURN_SDN_XML to a downloaded SDN publication to run")
	}

	// Reference: bench/ofac in -index-aliases mode (aliases as keys).
	dir := t.TempDir()
	refEntries := filepath.Join(dir, "ref-entries.jsonl")
	cmd := exec.Command("go", "run", "../bench/ofac",
		"-in", sdn, "-entries", refEntries, "-corpus", filepath.Join(dir, "ref-corpus.csv"), "-index-aliases")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bench/ofac: %v\n%s", err, out)
	}

	// Candidate: the shipped mapping.
	raw, err := os.ReadFile("../docs/examples/ofac.mapping.json")
	if err != nil {
		t.Fatal(err)
	}
	var m ingest.Mapping
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	an, err := engine.ResolveAnalyzer(m.List.Analyzer)
	if err != nil {
		t.Fatal(err)
	}
	analyzedSet := func(keys []string) map[string]struct{} {
		s := map[string]struct{}{}
		for _, k := range keys {
			if a := an.Normalize(k); a != "" {
				s[a] = struct{}{}
			}
		}
		return s
	}

	f, err := os.Open(sdn)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := map[string]map[string]struct{}{}
	if _, err := ingest.Parse(&m, f, ingest.Options{}, func(e engine.Entry) error {
		got[e.ID] = analyzedSet(e.Keys)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	ref := map[string]map[string]struct{}{}
	rf, err := os.Open(refEntries)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	dec := json.NewDecoder(rf)
	for dec.More() {
		var e engine.Entry
		if err := dec.Decode(&e); err != nil {
			t.Fatal(err)
		}
		ref[e.ID] = analyzedSet(e.Keys)
	}

	if len(got) != len(ref) {
		t.Fatalf("entry counts differ: mapping %d, extractor %d", len(got), len(ref))
	}
	diffs := 0
	for id, rset := range ref {
		gset, ok := got[id]
		if !ok {
			t.Errorf("entry %s missing from mapping output", id)
			diffs++
			continue
		}
		if fmt.Sprint(sortedKeys(rset)) != fmt.Sprint(sortedKeys(gset)) {
			if diffs < 5 {
				t.Errorf("entry %s analyzed keys differ:\n  extractor: %v\n  mapping:   %v",
					id, sortedKeys(rset), sortedKeys(gset))
			}
			diffs++
		}
	}
	if diffs > 0 {
		t.Fatalf("%d of %d entries differ", diffs, len(ref))
	}
	t.Logf("equivalent: %d entries, analyzed key sets identical", len(ref))
}

func sortedKeys(s map[string]struct{}) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
