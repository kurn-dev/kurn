package ingest_test

// Bundle build — determinism, the version_id == node-stamp
// identity, delta correctness, duplicate-ID rejection.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/ingest"
)

const feedV1 = `{"id":"a","k":"Anna Smith"}
{"id":"b","k":"Bo Chan"}
{"id":"c","k":"Cid Voss"}`

const feedV2 = `{"id":"a","k":"Anna Smythe"}
{"id":"b","k":"Bo Chan"}
{"id":"d","k":"Dee Falk"}`

func ndjsonMapping() *ingest.Mapping {
	return &ingest.Mapping{Format: "ndjson", ID: "id", Keys: []ingest.KeyRule{{Path: "k"}}, List: exactList()}
}

func buildTo(t *testing.T, feed, out string, opts ingest.BuildOptions) *ingest.Manifest {
	t.Helper()
	man, err := ingest.Build(ndjsonMapping(), strings.NewReader(feed), out, opts)
	if err != nil {
		t.Fatal(err)
	}
	return man
}

func TestBuildBundleAndVersionIdentity(t *testing.T) {
	out := filepath.Join(t.TempDir(), "bundle")
	man := buildTo(t, feedV1, out, ingest.BuildOptions{Source: "unit-fixture"})
	for _, f := range []string{"config.json", "base.jsonl", "base.idx", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Fatalf("bundle missing %s: %v", f, err)
		}
	}
	if man.Entries != 3 || man.Keys != 3 || man.Mode != "exact" || man.V != 1 {
		t.Fatalf("manifest: %+v", man)
	}
	if len(man.SHA256) != 64 || man.VersionID != man.SHA256[:12] {
		t.Fatalf("identity fields: %+v", man)
	}
	if man.Analyzer == "" {
		t.Fatal("analyzer digest missing")
	}

	// THE identity property: dropped into a store as a list dir, the node
	// reports exactly this version.
	dataDir := t.TempDir()
	if err := os.Rename(out, filepath.Join(dataDir, "codes")); err != nil {
		t.Fatal(err)
	}
	st, err := engine.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Skipped) != 0 {
		t.Fatalf("bundle dir skipped: %+v", st.Skipped)
	}
	l, ok := st.List("codes")
	if !ok {
		t.Fatal("bundle list missing")
	}
	if !strings.HasPrefix(l.Version(), man.VersionID+"@") {
		t.Fatalf("node version %q does not carry manifest version_id %q", l.Version(), man.VersionID)
	}
	if c := l.Query("anna smith", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "a" {
		t.Fatalf("bundle content wrong: %+v", c)
	}
}

func TestBuildDeterministic(t *testing.T) {
	d := t.TempDir()
	buildTo(t, feedV1, filepath.Join(d, "one"), ingest.BuildOptions{})
	buildTo(t, feedV1, filepath.Join(d, "two"), ingest.BuildOptions{})
	for _, f := range []string{"config.json", "base.jsonl", "base.idx", "manifest.json"} {
		a, _ := os.ReadFile(filepath.Join(d, "one", f))
		b, _ := os.ReadFile(filepath.Join(d, "two", f))
		if !bytes.Equal(a, b) {
			t.Fatalf("%s differs between identical builds", f)
		}
	}
}

func TestBuildDelta(t *testing.T) {
	d := t.TempDir()
	buildTo(t, feedV1, filepath.Join(d, "v1"), ingest.BuildOptions{})
	man := buildTo(t, feedV2, filepath.Join(d, "v2"), ingest.BuildOptions{PrevDir: filepath.Join(d, "v1")})
	if man.Delta == nil || man.Delta.Adds != 1 || man.Delta.Updates != 1 || man.Delta.Deletes != 1 {
		t.Fatalf("delta stats: %+v", man.Delta)
	}
	if man.PrevSHA256 == "" || man.PrevSHA256 == man.SHA256 {
		t.Fatalf("prev linkage: %+v", man)
	}
	raw, err := os.ReadFile(filepath.Join(d, "v2", "delta.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("delta lines: %v", lines)
	}
	// update a (new entry attached), add d (entry attached), delete c (id only).
	if !strings.Contains(lines[0], `"op":"update"`) || !strings.Contains(lines[0], "Anna Smythe") {
		t.Fatalf("line 0: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"op":"add"`) || !strings.Contains(lines[1], `"id":"d"`) {
		t.Fatalf("line 1: %s", lines[1])
	}
	if !strings.Contains(lines[2], `"op":"delete"`) || !strings.Contains(lines[2], `"id":"c"`) || strings.Contains(lines[2], "entry") {
		t.Fatalf("line 2: %s", lines[2])
	}
	// No-change delta: v2 against itself is empty.
	man2 := buildTo(t, feedV2, filepath.Join(d, "v2b"), ingest.BuildOptions{PrevDir: filepath.Join(d, "v2")})
	if *man2.Delta != (ingest.DeltaStats{}) {
		t.Fatalf("self-delta not empty: %+v", man2.Delta)
	}
}

func TestBuildRejectsDuplicateIDs(t *testing.T) {
	dup := feedV1 + "\n" + `{"id":"a","k":"Anna Again"}`
	_, err := ingest.Build(ndjsonMapping(), strings.NewReader(dup), filepath.Join(t.TempDir(), "out"), ingest.BuildOptions{})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate IDs accepted: %v", err)
	}
}
