package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine/prepared"
)

func artifactFixture(t *testing.T) (dir, mapping, feed string) {
	t.Helper()
	dir = t.TempDir()
	mapping = filepath.Join(dir, "mapping.json")
	feed = filepath.Join(dir, "feed.ndjson")
	if err := os.WriteFile(mapping, []byte(`{"format":"ndjson","id":"id","keys":[{"path":"name"}],"list":{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"exact"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(feed, []byte("{\"id\":\"1\",\"name\":\"Anna Smith\"}\n{\"id\":\"2\",\"name\":\"Bo Chan\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, mapping, feed
}

func TestBuildArtifactFlagRules(t *testing.T) {
	dir, mapping, feed := artifactFixture(t)
	art := filepath.Join(dir, "list.kurn")
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"artifact with out":     {[]string{"-mapping", mapping, "-in", feed, "-artifact", art, "-out", filepath.Join(dir, "b")}, "mutually exclusive"},
		"artifact with prev":    {[]string{"-mapping", mapping, "-in", feed, "-artifact", art, "-prev", filepath.Join(dir, "p")}, "-prev"},
		"name without artifact": {[]string{"-mapping", mapping, "-in", feed, "-out", filepath.Join(dir, "b"), "-name", "x"}, "-name applies only with -artifact"},
		"invalid name":          {[]string{"-mapping", mapping, "-in", feed, "-artifact", art, "-name", "Bad Name"}, "invalid"},
		"too long name":         {[]string{"-mapping", mapping, "-in", feed, "-artifact", art, "-name", strings.Repeat("a", 65)}, "invalid"},
		"invalid default":       {[]string{"-mapping", mapping, "-in", feed, "-artifact", filepath.Join(dir, "My List.kurn")}, "set -name"},
		"missing input":         {[]string{"-mapping", mapping, "-artifact", art}, "required"},
	} {
		err := cmdBuild(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
	// None of the refused invocations wrote anything.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if n := e.Name(); n != "mapping.json" && n != "feed.ndjson" {
			t.Fatalf("a refused build left %s", n)
		}
	}
}

func TestBuildArtifactDefaultAndExplicitName(t *testing.T) {
	dir, mapping, feed := artifactFixture(t)
	art := filepath.Join(dir, "products.kurn")
	if err := cmdBuild([]string{"-mapping", mapping, "-in", feed, "-artifact", art, "-source", "cli-test"}); err != nil {
		t.Fatal(err)
	}
	info, err := prepared.Inspect(art)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "products" || info.Entries != 2 || info.Mode != "exact" {
		t.Fatalf("inspect %+v", info)
	}
	fi, _ := os.Stat(art)
	if fi.Mode().Perm() != 0o444 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if _, err := prepared.Verify(art, prepared.LevelFull); err != nil {
		t.Fatal(err)
	}
	// Absent-only: a second build to the same file is refused.
	if err := cmdBuild([]string{"-mapping", mapping, "-in", feed, "-artifact", art}); err == nil {
		t.Fatal("rebuilt over an existing artifact")
	}
	named := filepath.Join(dir, "Weird Name.v2.kurn")
	if err := cmdBuild([]string{"-mapping", mapping, "-in", feed, "-artifact", named, "-name", "sanctions_v2"}); err != nil {
		t.Fatal(err)
	}
	if info, err := prepared.Inspect(named); err != nil || info.Name != "sanctions_v2" {
		t.Fatalf("inspect %+v, %v", info, err)
	}
	if got := defaultArtifactName("/x/y/list.v1.kurn"); got != "list.v1" {
		t.Fatalf("default name %q", got)
	}
}

func TestBuildArtifactFromStdin(t *testing.T) {
	dir, mapping, feed := artifactFixture(t)
	f, err := os.Open(feed)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = old }()
	art := filepath.Join(dir, "stdin.kurn")
	if err := cmdBuild([]string{"-mapping", mapping, "-in", "-", "-artifact", art}); err != nil {
		t.Fatal(err)
	}
	if info, err := prepared.Inspect(art); err != nil || info.Name != "stdin" || info.Entries != 2 {
		t.Fatalf("inspect %+v, %v", info, err)
	}
}

// TestBuildArtifactRefusesCoercibleDuplicate is F2 case (a) through the
// CLI: a valid row followed by an invalid-UTF-8 row that json.Marshal would
// coerce into a "byte-identical" duplicate. The build fails (main turns the
// error into exit status 1) and no output file or temporary is left.
func TestBuildArtifactRefusesCoercibleDuplicate(t *testing.T) {
	dir := t.TempDir()
	mapping := filepath.Join(dir, "mapping.json")
	feed := filepath.Join(dir, "feed.csv")
	if err := os.WriteFile(mapping, []byte(`{"format":"csv","id":"id","keys":[{"path":"name"}],"list":{"analyzer":{"preset":"identifier"},"match":{"mode":"exact"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(feed, []byte("id,name\n1,Bad�Name\n1,Bad\xffName\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	art := filepath.Join(out, "list.kurn")
	err := cmdBuild([]string{"-mapping", mapping, "-in", feed, "-artifact", art, "-skip-bad", "3"})
	if err == nil || !strings.Contains(err.Error(), "not valid UTF-8") || !strings.Contains(err.Error(), "-skip-bad does not apply") {
		t.Fatalf("got %v, want the UTF-8 refusal", err)
	}
	if ents, _ := os.ReadDir(out); len(ents) != 0 {
		t.Fatalf("refused build left %v", ents)
	}
}
