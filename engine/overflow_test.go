package engine_test

// Regression tests for the exact-mode hot-key overflow:
// >2^20-1 entries sharing one analyzed key used to panic in exact.Finish —
// journaled before the panic on the upsert path, so the poison was durable
// and Open panicked replaying it. Now: a typed *exact.KeyOverflowError
// before any state or disk change, and a poisoned journal is quarantined
// at Open instead of blocking startup.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/exact"
)

// hotEntries returns n entries with distinct IDs all sharing one analyzed
// key — n = 1<<20 is one over the packed run-length cap.
func hotEntries(n int) []engine.Entry {
	entries := make([]engine.Entry, n)
	for i := range entries {
		entries[i] = engine.Entry{ID: fmt.Sprintf("e%07d", i), Keys: []string{"hot"}}
	}
	return entries
}

// A hot-key upsert must be rejected with the typed error and leave the list
// exactly as it was — queryable, and NOT mutation-dead (a later valid upsert
// succeeds).
func TestListUpsertHotKeyRejected(t *testing.T) {
	l := exactList(t)
	if err := l.Upsert([]engine.Entry{{ID: "g1", Keys: []string{"good"}}}); err != nil {
		t.Fatal(err)
	}
	err := l.Upsert(hotEntries(1 << 20))
	if err == nil {
		t.Fatal("hot-key upsert accepted")
	}
	var ko *exact.KeyOverflowError
	if !errors.As(err, &ko) {
		t.Fatalf("error is %T (%v), want *exact.KeyOverflowError", err, err)
	}
	if ko.Key != "hot" {
		t.Errorf("KeyOverflowError.Key = %q, want %q", ko.Key, "hot")
	}
	// State intact: the good entry is still served, and the list is not
	// mutation-dead.
	if c := l.Query("good", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "g1" {
		t.Fatalf("good entry missing after rejected upsert: %+v", c)
	}
	if err := l.Upsert([]engine.Entry{{ID: "g2", Keys: []string{"also good"}}}); err != nil {
		t.Fatalf("list is mutation-dead after rejected upsert: %v", err)
	}
	if c := l.Query("also good", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "g2" {
		t.Fatalf("post-rejection upsert not live: %+v", c)
	}
	if e, o, _ := l.Stats(); e != 2 || o != 2 {
		t.Fatalf("stats after rejection + valid upsert: entries=%d overlay=%d, want 2,2", e, o)
	}
}

// Same for Replace: rejected, prior content untouched.
func TestListReplaceHotKeyRejected(t *testing.T) {
	l := exactList(t)
	if err := l.Replace([]engine.Entry{{ID: "g1", Keys: []string{"good"}}}); err != nil {
		t.Fatal(err)
	}
	err := l.Replace(hotEntries(1 << 20))
	var ko *exact.KeyOverflowError
	if !errors.As(err, &ko) {
		t.Fatalf("Replace error = %v, want *exact.KeyOverflowError", err)
	}
	if c := l.Query("good", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "g1" {
		t.Fatalf("prior content lost after rejected Replace: %+v", c)
	}
}

// The core durability regression: a rejected Store.Upsert must NOT have
// journaled the poison — the journal file is byte-identical afterwards, and
// a restart opens clean with no quarantine.
func TestStoreUpsertHotKeyNotJournaled(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "exact"},
	}
	if _, err := st.CreateList("codes", cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("codes", []engine.Entry{{ID: "g1", Keys: []string{"good"}}}); err != nil {
		t.Fatal(err)
	}
	jp := filepath.Join(dir, "codes", "journal.jsonl")
	before, err := os.ReadFile(jp)
	if err != nil {
		t.Fatal(err)
	}

	var ko *exact.KeyOverflowError
	if err := st.Upsert("codes", hotEntries(1<<20)); !errors.As(err, &ko) {
		t.Fatalf("Store.Upsert error = %v, want *exact.KeyOverflowError", err)
	}
	after, err := os.ReadFile(jp)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("rejected upsert was journaled — poison persisted")
	}

	// Restart: clean open, no quarantine, only the good entry present.
	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("Open after rejected upsert: %v", err)
	}
	if len(st2.Quarantined) != 0 {
		t.Fatalf("clean journal quarantined: %+v", st2.Quarantined)
	}
	l, ok := st2.List("codes")
	if !ok {
		t.Fatal("list missing after reopen")
	}
	if c := l.Query("good", engine.QueryOpts{}); len(c) != 1 {
		t.Fatalf("journaled good entry missing after reopen: %+v", c)
	}
	if c := l.Query("hot", engine.QueryOpts{}); len(c) != 0 {
		t.Fatalf("poison present after reopen: %+v", c)
	}
	if _, err := os.Stat(jp + ".quarantined"); !os.IsNotExist(err) {
		t.Fatal("quarantine file exists for a clean journal")
	}
}

// A poisoned journal (written before build-time validation existed, or
// hand-crafted) must not block startup: Open quarantines it aside, records
// it in the store's Quarantined report, and the list opens at its base
// state. A second Open is equally clean (idempotent quarantine slot).
func TestOpenQuarantinesPoisonedJournal(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "exact"},
	}
	if _, err := st.CreateList("codes", cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("codes", []engine.Entry{{ID: "c1", Keys: []string{"AA-1"}}}); err != nil {
		t.Fatal(err)
	}

	// Hand-craft the poison: 1<<20 upsert records, distinct IDs, one key.
	jp := filepath.Join(dir, "codes", "journal.jsonl")
	f, err := os.Create(jp)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	for i := 0; i < 1<<20; i++ {
		fmt.Fprintf(w, `{"op":"upsert","entry":{"id":"e%07d","keys":["hot"]}}`+"\n", i)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("poisoned journal blocked startup: %v", err)
	}
	if len(st2.Quarantined) != 1 {
		t.Fatalf("Quarantined = %+v, want one entry", st2.Quarantined)
	}
	q := st2.Quarantined[0]
	if q.List != "codes" {
		t.Fatalf("quarantined list = %q, want codes", q.List)
	}
	var ko *exact.KeyOverflowError
	if !errors.As(q.Err, &ko) {
		t.Fatalf("quarantine error is %T (%v), want *exact.KeyOverflowError", q.Err, q.Err)
	}
	wantPath := jp + ".quarantined"
	if q.Path != wantPath {
		t.Fatalf("quarantine path = %q, want %q", q.Path, wantPath)
	}
	if _, err := os.Stat(jp); !os.IsNotExist(err) {
		t.Fatal("poisoned journal still in place")
	}
	if fi, err := os.Stat(wantPath); err != nil || fi.Size() == 0 {
		t.Fatalf("quarantined journal missing/empty: %v", err)
	}

	l, ok := st2.List("codes")
	if !ok {
		t.Fatal("list missing after quarantine")
	}
	if c := l.Query("aa-1", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "c1" {
		t.Fatalf("base entry not served after quarantine: %+v", c)
	}
	if c := l.Query("hot", engine.QueryOpts{}); len(c) != 0 {
		t.Fatalf("poisoned entries live after quarantine: %+v", c)
	}

	// Idempotent: a second Open finds no journal, quarantines nothing.
	st3, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if len(st3.Quarantined) != 0 {
		t.Fatalf("second Open quarantined %+v, want none", st3.Quarantined)
	}
}
