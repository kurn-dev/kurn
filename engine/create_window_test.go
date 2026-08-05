package engine_test

// Regression tests: The CreateList crash window and
// Open's fatal-on-any-bad-dir behavior. Pre-fix, a crash between the data
// wipe and the config write left either a config-less dir (first create;
// Open then refused the WHOLE store) or an old config over wiped data
// (PUT-replace; the list silently reverted). Now CreateList brackets the
// window with a .creating marker, and Open skips-and-reports invalid list
// dirs instead of refusing the store.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func exactCfg() engine.ListConfig {
	return engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "exact"},
	}
}

// A stray subdir (external tooling, editor droppings) must not block the
// store: Open serves the valid lists and reports the bad dir.
func TestOpenSkipsInvalidListDirs(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("codes", exactCfg()); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("codes", []engine.Entry{{ID: "c1", Keys: []string{"AA-1"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "stray"), 0o755); err != nil {
		t.Fatal(err)
	}

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("stray dir blocked the store: %v", err)
	}
	if len(st2.Skipped) != 1 || st2.Skipped[0].List != "stray" {
		t.Fatalf("Skipped = %+v, want one entry for %q", st2.Skipped, "stray")
	}
	if st2.Skipped[0].Err == nil {
		t.Fatal("skip reason missing")
	}
	l, ok := st2.List("codes")
	if !ok {
		t.Fatal("valid list not served alongside a stray dir")
	}
	if c := l.Query("aa-1", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "c1" {
		t.Fatalf("valid list content wrong: %+v", c)
	}
	if _, ok := st2.List("stray"); ok {
		t.Fatal("stray dir served as a list")
	}
}

// A crash inside the CreateList window (marker present) must not serve the
// half-written list — neither the old config over wiped data (silent revert)
// nor a config-less dir (store brick). The list is skipped with a reason,
// siblings are served, and a re-PUT repairs it.
func TestOpenSkipsInterruptedCreate(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("codes", exactCfg()); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("codes", []engine.Entry{{ID: "c1", Keys: []string{"AA-1"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("people", exactCfg()); err != nil {
		t.Fatal(err)
	}
	// Simulate the crash: marker present, as if a PUT-replace died mid-window.
	if err := os.WriteFile(filepath.Join(dir, "codes", ".creating"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("interrupted create blocked the store: %v", err)
	}
	if len(st2.Skipped) != 1 || st2.Skipped[0].List != "codes" {
		t.Fatalf("Skipped = %+v, want one entry for %q", st2.Skipped, "codes")
	}
	if _, ok := st2.List("codes"); ok {
		t.Fatal("interrupted list served despite the marker")
	}
	if _, ok := st2.List("people"); !ok {
		t.Fatal("sibling list not served")
	}

	// Repair path: a fresh PUT completes the replace and clears the marker.
	if _, err := st2.CreateList("codes", exactCfg()); err != nil {
		t.Fatalf("re-PUT repair failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "codes", ".creating")); !os.IsNotExist(err) {
		t.Fatal("marker survived a successful CreateList")
	}
	if err := st2.Upsert("codes", []engine.Entry{{ID: "c2", Keys: []string{"BB-2"}}}); err != nil {
		t.Fatal(err)
	}
	st3, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st3.Skipped) != 0 {
		t.Fatalf("repaired store still skipping: %+v", st3.Skipped)
	}
	l, _ := st3.List("codes")
	if c := l.Query("bb-2", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "c2" {
		t.Fatalf("repaired list content wrong: %+v", c)
	}
}

// A successful CreateList must leave no marker behind (the window closed).
func TestCreateListLeavesNoMarker(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("codes", exactCfg()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "codes", ".creating")); !os.IsNotExist(err) {
		t.Fatal("marker present after successful CreateList")
	}
}

// The marker brackets the wipe, so it must be on disk BEFORE the first
// destructive step — otherwise a crash inside the window leaves the old
// config over wiped data, the exact silent revert the marker prevents.
//
// fsync itself is not observable in-process, so this pins the half that is:
// the ordering. The wipe is forced to fail (a data file replaced by a
// non-empty directory, which os.Remove refuses with ENOTEMPTY) and the
// marker must already be present when it does — then Open must skip the
// list rather than serve its stale config.
func TestCreateListMarkerPrecedesWipe(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("codes", exactCfg()); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("codes", []engine.Entry{{ID: "c1", Keys: []string{"AA-1"}}}); err != nil {
		t.Fatal(err)
	}

	// Make the base file undeletable: a directory with a child in it.
	base := filepath.Join(dir, "codes", "base.jsonl")
	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The replace must fail in the wipe, after the marker is written.
	if _, err := st.CreateList("codes", exactCfg()); err == nil {
		t.Fatal("CreateList succeeded despite an undeletable base file")
	}
	if _, err := os.Stat(filepath.Join(dir, "codes", ".creating")); err != nil {
		t.Fatalf("marker absent after a failed wipe — the window is unbracketed: %v", err)
	}

	// With the marker down, a reopen refuses to serve the indeterminate list
	// instead of falling back to its previous config.
	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("interrupted create blocked the whole store: %v", err)
	}
	if _, ok := st2.List("codes"); ok {
		t.Fatal("list with a live marker was served")
	}
	if len(st2.Skipped) != 1 || st2.Skipped[0].List != "codes" {
		t.Fatalf("Skipped = %+v, want one entry for codes", st2.Skipped)
	}
}
