package engine_test

// Regression test: .base-* / .cfg-* temp files stranded
// by crashed writes accumulated silently — Open now sweeps them.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestOpenSweepsOrphanedTempFiles(t *testing.T) {
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
	// Strand crash survivors.
	lp := filepath.Join(dir, "codes")
	for _, f := range []string{".base-1234567", ".cfg-7654321"} {
		if err := os.WriteFile(filepath.Join(lp, f), []byte("orphan"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".base-1234567", ".cfg-7654321"} {
		if _, err := os.Stat(filepath.Join(lp, f)); !os.IsNotExist(err) {
			t.Errorf("orphaned %s survived Open", f)
		}
	}
	// The list itself is untouched.
	l, ok := st2.List("codes")
	if !ok {
		t.Fatal("list missing")
	}
	if c := l.Query("aa-1", engine.QueryOpts{}); len(c) != 1 {
		t.Fatalf("list content wrong after sweep: %+v", c)
	}
	if len(st2.Skipped) != 0 {
		t.Fatalf("sweep produced skips: %+v", st2.Skipped)
	}
}
