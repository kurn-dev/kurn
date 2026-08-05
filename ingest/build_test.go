package ingest_test

// Bundle build — determinism, full-sha256 node-stamp provenance,
// delta correctness, duplicate-ID rejection.

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
	// The stamp's base half is the manifest's full sha256; the short
	// version_id keeps its join property as a prefix of both.
	if !strings.HasPrefix(l.Version(), man.SHA256+"@") {
		t.Fatalf("node version %q does not carry the manifest sha256 %q", l.Version(), man.SHA256)
	}
	if !strings.HasPrefix(l.Version(), man.VersionID) {
		t.Fatalf("node version %q does not start with version_id %q", l.Version(), man.VersionID)
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

// A failed build must leave a directory the retry can use. An earlier
// version renamed the store files into outDir and then built the delta in
// place: a delta failure left base.jsonl behind, and the next Build refused
// the directory as "already contains a bundle" — blocking exactly the retry
// the failure invited. Everything now stages first, and base.jsonl (the
// guard's own sentinel) publishes last.
func TestFailedDeltaLeavesARetryableDirectory(t *testing.T) {
	d := t.TempDir()
	mp := &ingest.Mapping{Format: "csv", ID: "id",
		Keys: []ingest.KeyRule{{Path: "name"}}, List: exactList()}
	feed := "id,name\n1,Anna\n2,Bob\n"

	// A prev bundle whose base.jsonl is corrupt: fileSHA256 passes over it,
	// writeDelta's line parse fails — the delta phase, specifically.
	prev := filepath.Join(d, "prev")
	if err := os.MkdirAll(prev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prev, "base.jsonl"), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(d, "out")
	_, err := ingest.Build(mp, strings.NewReader(feed), out, ingest.BuildOptions{PrevDir: prev})
	if err == nil {
		t.Fatal("a corrupt prev bundle produced a delta")
	}

	// The decisive property: nothing published, so the same directory
	// works on retry — here with the prev repaired.
	if _, serr := os.Stat(filepath.Join(out, "base.jsonl")); !os.IsNotExist(serr) {
		t.Fatalf("base.jsonl left behind by the failed build (stat err: %v)", serr)
	}
	if err := os.WriteFile(filepath.Join(prev, "base.jsonl"),
		[]byte(`{"id":"1","keys":["anna"]}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	man, err := ingest.Build(mp, strings.NewReader(feed), out, ingest.BuildOptions{PrevDir: prev})
	if err != nil {
		t.Fatalf("retry after a failed delta was refused: %v", err)
	}
	if man.Entries != 2 || man.Delta == nil || man.Delta.Adds != 1 {
		t.Fatalf("retry produced a wrong bundle: %+v", man)
	}
	for _, f := range []string{"config.json", "base.jsonl", "base.idx", "manifest.json", "delta.jsonl"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Fatalf("retry bundle missing %s: %v", f, err)
		}
	}
}

// An interrupted delta-enabled attempt leaves delta.jsonl behind; a retry
// WITHOUT PrevDir publishes no delta, so the leftover would sit beside a
// manifest that knows nothing of it — and convention-driven consumers read
// the file, not the manifest. The no-delta publish must remove it.
func TestNoDeltaRetryRemovesAStaleDelta(t *testing.T) {
	d := t.TempDir()
	mp := &ingest.Mapping{Format: "csv", ID: "id",
		Keys: []ingest.KeyRule{{Path: "name"}}, List: exactList()}

	out := filepath.Join(d, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "delta.jsonl"),
		[]byte(`{"op":"add","id":"stale"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	man, err := ingest.Build(mp, strings.NewReader("id,name\n1,Anna\n"), out, ingest.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if man.Delta != nil {
		t.Fatalf("no-delta build reported delta stats: %+v", man.Delta)
	}
	if _, err := os.Stat(filepath.Join(out, "delta.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("stale delta.jsonl survived a no-delta publish (stat err: %v)", err)
	}
}
