package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildReadsStdinWhenInputIsDash(t *testing.T) {
	dir := t.TempDir()
	mapping := filepath.Join(dir, "mapping.json")
	feed := filepath.Join(dir, "feed.ndjson")
	if err := os.WriteFile(mapping, []byte(`{"format":"ndjson","id":"id","keys":[{"path":"name"}],"list":{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"exact"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(feed, []byte("{\"id\":\"1\",\"name\":\"Anna Smith\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(feed)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = old }()
	out := filepath.Join(dir, "bundle")
	if err := cmdBuild([]string{"-mapping", mapping, "-in", "-", "-out", out, "-source", "stdin-test"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.json", "base.jsonl", "base.idx", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatalf("bundle missing %s: %v", name, err)
		}
	}
}
