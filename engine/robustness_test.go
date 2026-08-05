package engine_test

// Three startup-robustness gaps: a temp prefix the crash sweep never
// knew about, a journal reader that judged a record's size only after
// materializing it, and list config values nothing validated.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kurn-dev/kurn/engine"
)

func robustList() engine.ListConfig {
	return engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram"},
	}
}

// artifact.Save writes through a .idx-* temp, but the crash sweep knew
// only .base-* and .cfg-*. Nothing else ever looks at these files, so a
// crash mid-save leaked one per crash, forever.
func TestOpenSweepsEveryTempPrefix(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("people", robustList()); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("people", []engine.Entry{{ID: "1", Keys: []string{"Anna Smith"}}}); err != nil {
		t.Fatal(err)
	}

	// One survivor per CreateTemp prefix used under a list directory.
	lp := filepath.Join(dir, "people")
	for _, name := range []string{".base-crash", ".cfg-crash", ".idx-crash"} {
		if err := os.WriteFile(filepath.Join(lp, name), []byte("orphan"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = engine.Open(dir); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(lp, ".*-crash"))
	if len(left) > 0 {
		t.Fatalf("Open left crash temp files behind: %v", left)
	}
}

// An oversize journal record failed openList outright, so the whole list
// vanished from the store — harsher than the quarantine a merely
// unparseable journal gets, for a file that is corrupt either way.
func TestOversizeJournalRecordKeepsTheList(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("people", robustList()); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("people", []engine.Entry{{ID: "keep", Keys: []string{"Anna Smith"}}}); err != nil {
		t.Fatal(err)
	}

	// Append a well-formed but oversize record after the good one.
	jp := filepath.Join(dir, "people", "journal.jsonl")
	f, err := os.OpenFile(jp, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, `{"op":"upsert","entry":{"id":"huge","keys":[%q]}}`+"\n", strings.Repeat("x", 2<<20))
	f.Close()

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("an oversize journal record failed Open: %v", err)
	}
	l, ok := st2.List("people")
	if !ok {
		t.Fatal("the list was dropped entirely — an oversize record must not cost more than its own tail")
	}
	// The clean prefix survives; the oversize record and everything after
	// it does not.
	if entries, _, _ := l.Stats(); entries != 1 {
		t.Fatalf("live entries = %d, want 1 (the record before the corruption)", entries)
	}
	// And the file is truncated, so the next append cannot glue itself to
	// the discarded tail.
	fi, err := os.Stat(jp)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 1<<20 {
		t.Fatalf("journal still %d bytes — the corrupt tail was not truncated away", fi.Size())
	}
}

// The size check ran after ReadBytes had already materialized the record,
// so a newline-less tail was read whole at Open purely to be discarded.
func TestJournalTailIsNotMaterializedToBeDiscarded(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("people", robustList()); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("people", []engine.Entry{{ID: "keep", Keys: []string{"Anna Smith"}}}); err != nil {
		t.Fatal(err)
	}

	// 32 MiB of newline-less garbage: a torn write, discarded on sight.
	jp := filepath.Join(dir, "people", "journal.jsonl")
	f, err := os.OpenFile(jp, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.Repeat("x", 1<<20)
	for i := 0; i < 32; i++ {
		if _, err := f.WriteString(blob); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	st2, err := engine.Open(dir)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := st2.List("people")
	if !ok {
		t.Fatal("the list was dropped")
	}
	if entries, _, _ := l.Stats(); entries != 1 {
		t.Fatalf("live entries = %d, want 1 — the clean prefix did not survive the torn tail", entries)
	}
	// Reading stops at the 1 MiB bound, so Open never holds the tail.
	const ceiling = 8 << 20
	if got := after.TotalAlloc - before.TotalAlloc; got > ceiling {
		t.Fatalf("Open allocated %d bytes against a 32 MiB torn tail, over the %d ceiling", got, ceiling)
	}
}

// Threshold/TopK carry a negative sentinel per QUERY, never per list, and
// nothing checked that. A hand-edited config.json loaded straight into the
// no-floor maximal-scan, unlimited-collection shape.
func TestListConfigRejectsOutOfRangeMatchDefaults(t *testing.T) {
	cases := []struct {
		name string
		cfg  func(*engine.ListConfig)
		want string
	}{
		{"negative threshold", func(c *engine.ListConfig) { c.Match.Threshold = -1 }, "threshold"},
		{"threshold above one", func(c *engine.ListConfig) { c.Match.Threshold = 1.5 }, "threshold"},
		{"negative topk", func(c *engine.ListConfig) { c.Match.TopK = -5 }, "topk"},
		{"topk that overflows the segment cut", func(c *engine.ListConfig) { c.Match.TopK = int(^uint(0) >> 1) }, "topk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := robustList()
			tc.cfg(&cfg)
			_, err := engine.NewList("people", cfg)
			if err == nil {
				t.Fatalf("config accepted: %+v", cfg.Match)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error does not name %s: %v", tc.want, err)
			}
		})
	}

	// Exact mode keeps 0/0 as its documented defaults, and both modes
	// still accept the values a real config carries.
	for _, cfg := range []engine.ListConfig{
		robustList(),
		{Analyzer: engine.AnalyzerConfig{Preset: "person-name"}, Match: engine.MatchConfig{Mode: "exact"}},
		{Analyzer: engine.AnalyzerConfig{Preset: "person-name"}, Match: engine.MatchConfig{Mode: "ngram", Threshold: 1, TopK: 1}},
	} {
		if _, err := engine.NewList("people", cfg); err != nil {
			t.Fatalf("valid config refused (%+v): %v", cfg.Match, err)
		}
	}
}

// Two *engine.Store on one data directory silently destroy data: the
// abandoned one's auto-compaction folds ITS view and truncates the
// journal, discarding operations the other store already acknowledged.
// kurnd could produce exactly that by dropping a tenant and adding it
// back (the store object outlived the registry entry), so a store must be
// able to release its claim on a directory.
func TestCloseEndsAllWritesToTheDirectory(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := robustList()
	cfg.OverlayAutoCompact = 4 // compact early and often
	if _, err := st.CreateList("people", cfg); err != nil {
		t.Fatal(err)
	}
	// Enough mutations that auto-compactions are in flight at Close.
	for i := 0; i < 400; i++ {
		if err := st.Upsert("people", []engine.Entry{
			{ID: fmt.Sprintf("e%d", i), Keys: []string{fmt.Sprintf("Person Number%d", i)}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Whatever is on disk when Close returns is final: a compaction still
	// running would rewrite base.jsonl and truncate the journal under the
	// store that reopens the directory next.
	snapshot := dirFingerprint(t, filepath.Join(dir, "people"))
	time.Sleep(250 * time.Millisecond)
	if after := dirFingerprint(t, filepath.Join(dir, "people")); after != snapshot {
		t.Fatalf("the directory changed after Close returned:\n before %s\n after  %s", snapshot, after)
	}

	// And the closed store refuses every mutation, so an in-flight request
	// holding it cannot write to a directory another store now owns.
	e := []engine.Entry{{ID: "x", Keys: []string{"Anna Smith"}}}
	for name, err := range map[string]error{
		"Upsert":     st.Upsert("people", e),
		"Replace":    st.Replace("people", e),
		"Delete":     st.Delete("people", "e1"),
		"Compact":    st.Compact("people"),
		"CreateList": errOf(st.CreateList("other", robustList())),
		"ReloadList": errOf(st.ReloadList("people")),
	} {
		if !errors.Is(err, engine.ErrStoreClosed) {
			t.Errorf("%s on a closed store: err = %v, want ErrStoreClosed", name, err)
		}
	}

	// Reads still work: in-flight queries must be allowed to finish.
	if _, ok := st.List("people"); !ok {
		t.Error("Close broke reads on live *List objects")
	}

	// The reopened store sees exactly the acknowledged state.
	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := st2.List("people")
	if !ok {
		t.Fatal("reopen lost the list")
	}
	if entries, _, _ := l.Stats(); entries != 400 {
		t.Fatalf("reopened with %d entries, want the 400 acknowledged", entries)
	}
}

func errOf[T any](_ T, err error) error { return err }

// dirFingerprint renders every file's name and size, which is what a
// compaction changes: a new base and a truncated journal.
func dirFingerprint(t *testing.T, dir string) string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil {
			continue // raced with nothing, but a vanished file is itself a change
		}
		parts = append(parts, fmt.Sprintf("%s:%d", e.Name(), fi.Size()))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}
