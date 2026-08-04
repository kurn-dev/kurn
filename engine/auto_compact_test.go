package engine_test

// Regression tests: The design promised compaction
// "triggered by overlay-size threshold or the compact endpoint" — only the
// endpoint existed, so ungoverned overlay growth (O(overlay) rebuild per
// mutation) was the first wall a write-heavy user hit, silently.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kurn-dev/kurn/engine"
)

func TestOverlayAutoCompactTriggers(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := exactCfg()
	cfg.OverlayAutoCompact = 3
	if _, err := st.CreateList("codes", cfg); err != nil {
		t.Fatal(err)
	}
	for _, e := range []engine.Entry{
		{ID: "c1", Keys: []string{"AA-1"}},
		{ID: "c2", Keys: []string{"BB-2"}},
		{ID: "c3", Keys: []string{"CC-3"}},
	} {
		if err := st.Upsert("codes", []engine.Entry{e}); err != nil {
			t.Fatal(err)
		}
	}
	l, _ := st.List("codes")

	// The trigger runs off the mutation path: poll for the fold. Wait for
	// base.idx too — the artifact save is Compact's LAST disk write, so its
	// presence means the background goroutine is done touching the dir
	// (otherwise TempDir cleanup races the fold's writes).
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, ov, _ := l.Stats()
		_, idxErr := os.Stat(filepath.Join(dir, "codes", "base.idx"))
		if ov == 0 && idxErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("auto-compact never finished: overlay %d, base.idx err %v", ov, idxErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e, _, tomb := l.Stats(); e != 3 || tomb != 0 {
		t.Fatalf("post-fold stats: entries %d tombstones %d, want 3, 0", e, tomb)
	}
	if v := l.Version(); !strings.HasSuffix(v, "@3+j0") {
		t.Fatalf("post-fold version %q, want content stamp @3+j0", v)
	}
	// All three entries survive the fold and are queryable.
	for _, q := range []string{"aa-1", "bb-2", "cc-3"} {
		if c := l.Query(q, engine.QueryOpts{}); len(c) != 1 {
			t.Fatalf("entry for %q missing after auto-compact: %+v", q, c)
		}
	}
}

func TestOverlayAutoCompactDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("codes", exactCfg()); err != nil {
		t.Fatal(err)
	}
	for i, k := range []string{"AA-1", "BB-2", "CC-3", "DD-4", "EE-5"} {
		if err := st.Upsert("codes", []engine.Entry{{ID: string(rune('a' + i)), Keys: []string{k}}}); err != nil {
			t.Fatal(err)
		}
	}
	// Give a wrongly-armed trigger a moment to fire, then assert it didn't.
	time.Sleep(50 * time.Millisecond)
	l, _ := st.List("codes")
	if _, ov, _ := l.Stats(); ov != 5 {
		t.Fatalf("overlay = %d, want 5 (auto-compact must be off by default)", ov)
	}
}

func TestOverlayAutoCompactRejectsNegative(t *testing.T) {
	cfg := exactCfg()
	cfg.OverlayAutoCompact = -1
	if _, err := engine.NewList("codes", cfg); err == nil {
		t.Fatal("negative overlay_auto_compact accepted")
	}
}
