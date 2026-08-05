package engine_test

// Regression test: Store.Compact used to fold memory
// BEFORE any disk write, so a failed persist left memory serving the folded
// corpus (shifted segment-local IDF scores) while disk kept the old base +
// full journal. Now the fold is prepared, persisted, and only then swapped —
// a failed persist leaves the list exactly as it was.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestCompactPersistFailureLeavesMemoryUntouched(t *testing.T) {
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
	if err := st.Upsert("codes", []engine.Entry{{ID: "c2", Keys: []string{"BB-2"}}}); err != nil {
		t.Fatal(err)
	}
	l, _ := st.List("codes")
	vBefore := l.Version()
	_, ovBefore, _ := l.Stats()
	if ovBefore != 1 {
		t.Fatalf("overlay = %d, want 1", ovBefore)
	}

	// Make the list dir unwritable: persistBase's CreateTemp fails.
	lp := filepath.Join(dir, "codes")
	if err := os.Chmod(lp, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(lp, 0o755) })

	if err := st.Compact("codes"); err == nil {
		t.Fatal("Compact succeeded with an unwritable dir")
	}
	// Memory untouched: same version, same overlay, both entries served.
	if v := l.Version(); v != vBefore {
		t.Fatalf("failed Compact mutated memory: version %q -> %q", vBefore, v)
	}
	if _, ov, _ := l.Stats(); ov != ovBefore {
		t.Fatalf("failed Compact folded the overlay: %d -> %d", ovBefore, ov)
	}

	// Repair and compact for real: folded base, empty overlay, hash stamp.
	if err := os.Chmod(lp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := st.Compact("codes"); err != nil {
		t.Fatal(err)
	}
	if _, ov, _ := l.Stats(); ov != 0 {
		t.Fatalf("post-compact overlay = %d, want 0", ov)
	}
	if v := l.Version(); !strings.Contains(v, "@2+j0+c") || v == vBefore {
		t.Fatalf("post-compact version %q, want content stamp @2+j0+c…", v)
	}
	if c := l.Query("bb-2", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "c2" {
		t.Fatalf("folded entry missing: %+v", c)
	}
}
