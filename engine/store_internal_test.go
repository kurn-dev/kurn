package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kurn-dev/kurn/engine/exact"
	"github.com/kurn-dev/kurn/engine/ngram"
)

func internalPersonCfg() ListConfig {
	return ListConfig{
		Analyzer: AnalyzerConfig{Preset: "person-name"},
		Match:    MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, Threshold: 0.6, TopK: 100},
	}
}

// TestAppendJournalPartialWriteRolledBack: a journal append that fails midway
// must truncate the file back to its pre-append size, so the fragment cannot
// glue onto the NEXT acknowledged append (which the following restart's
// torn-tail repair would then drop).
func TestAppendJournalPartialWriteRolledBack(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("people", internalPersonCfg()); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("people", []Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}}); err != nil {
		t.Fatal(err)
	}
	jp := filepath.Join(dir, "people", "journal.jsonl")
	before, err := os.Stat(jp)
	if err != nil {
		t.Fatal(err)
	}

	fileWrite = func(f *os.File, b []byte) (int, error) {
		n, _ := f.Write(b[:len(b)/2]) // really write half, then fail
		return n, errors.New("injected partial write")
	}
	defer func() { fileWrite = (*os.File).Write }()
	if err := st.Upsert("people", []Entry{{ID: "p2", Keys: []string{"Dana Kovak"}}}); err == nil {
		t.Fatal("partial write reported success")
	}
	fileWrite = (*os.File).Write

	after, err := os.Stat(jp)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("journal not rolled back: %d bytes, want %d", after.Size(), before.Size())
	}

	// The next acknowledged append must survive a restart. (Name chosen
	// gram-disjoint from the rolled-back "Dana Kovak": one shared gram
	// would score a spurious 100 via the known-gram denominator.)
	if err := st.Upsert("people", []Entry{{ID: "p3", Keys: []string{"Iris Bell"}}}); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := st2.List("people")
	if c := l.Query("iris bell", QueryOpts{}); len(c) != 1 || c[0].EntryID != "p3" {
		t.Fatalf("acknowledged append after rollback lost: %+v", c)
	}
	if c := l.Query("dana kovak", QueryOpts{}); len(c) != 0 {
		t.Fatalf("failed append leaked: %+v", c)
	}
	if e, _, _ := l.Stats(); e != 2 {
		t.Fatalf("entries = %d, want 2 (p1, p3)", e)
	}
}

// TestJournalDamagedRefusesAppends: when a failed append's rollback ALSO
// fails, the file may hold bytes this process never acknowledged — so the
// version stamp's running journal hash can no longer be trusted to match the
// file, and the torn fragment would glue onto the next append. Appends must
// be refused with ErrJournalDamaged (reads keep serving), and an operation
// that rebuilds the journal wholesale (Replace) repairs.
func TestJournalDamagedRefusesAppends(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("people", internalPersonCfg()); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("people", []Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}}); err != nil {
		t.Fatal(err)
	}

	fileWrite = func(f *os.File, b []byte) (int, error) {
		n, _ := f.Write(b[:len(b)/2]) // really write half, then fail
		return n, errors.New("injected partial write")
	}
	fileTruncate = func(f *os.File, size int64) error {
		return errors.New("injected rollback failure")
	}
	defer func() {
		fileWrite = (*os.File).Write
		fileTruncate = (*os.File).Truncate
	}()
	if err := st.Upsert("people", []Entry{{ID: "p2", Keys: []string{"Dana Kovak"}}}); err == nil {
		t.Fatal("append with failed write reported success")
	}
	fileWrite = (*os.File).Write
	fileTruncate = (*os.File).Truncate

	// The journal now holds a fragment this process cannot vouch for:
	// further appends must be refused, not stamped with a hash the file
	// doesn't match.
	err = st.Upsert("people", []Entry{{ID: "p3", Keys: []string{"Iris Bell"}}})
	if !errors.Is(err, ErrJournalDamaged) {
		t.Fatalf("append on a damaged journal: err=%v, want ErrJournalDamaged", err)
	}
	if err := st.Delete("people", "p1"); !errors.Is(err, ErrJournalDamaged) {
		t.Fatalf("delete on a damaged journal: err=%v, want ErrJournalDamaged", err)
	}
	// Reads keep working on the acknowledged state.
	l, _ := st.List("people")
	if c := l.Query("marcus chen", QueryOpts{}); len(c) != 1 || c[0].EntryID != "p1" {
		t.Fatalf("damaged journal broke reads: %+v", c)
	}

	// Replace rebuilds the journal wholesale (new base, truncated journal):
	// the damage is repaired and appends work again.
	if err := st.Replace("people", []Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}}); err != nil {
		t.Fatalf("Replace on a damaged journal: %v", err)
	}
	if err := st.Upsert("people", []Entry{{ID: "p3", Keys: []string{"Iris Bell"}}}); err != nil {
		t.Fatalf("append after repair: %v", err)
	}
	v := l.Version()

	// The repaired stamps are honest: a restart reproduces them from disk.
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l2, _ := st2.List("people")
	if got := l2.Version(); got != v {
		t.Fatalf("post-repair version %q not reproduced across restart (got %q)", v, got)
	}
	if c := l2.Query("iris bell", QueryOpts{}); len(c) != 1 || c[0].EntryID != "p3" {
		t.Fatalf("post-repair append lost across restart: %+v", c)
	}
}

// TestSaveArtifactNonFatal: a failed base.idx save must not panic or fail the
// operation (the artifact is a pure cache with a rebuild fallback); it is
// reported through OnArtifactError.
func TestSaveArtifactNonFatal(t *testing.T) {
	dir := t.TempDir()
	st := &Store{dir: dir}
	var gotList string
	var gotErr error
	st.OnArtifactError = func(list string, err error) { gotList, gotErr = list, err }

	lp := filepath.Join(dir, "people")
	// Make base.idx an occupied directory: artifact.Save's rename must fail.
	if err := os.MkdirAll(filepath.Join(lp, idxFile, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := NewList("people", ListConfig{
		Analyzer: AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    MatchConfig{Mode: "ngram"},
	})
	if err != nil {
		t.Fatal(err)
	}
	b := ngram.NewBuilder(ngram.Config{Grams: []int{2, 3}, StripSpaces: true})
	b.Add(0, []string{"marcus chen"})
	idx := b.Finish()
	st.saveArtifact(l, lp, idx, nil)

	if gotErr == nil || gotList != "people" {
		t.Fatalf("OnArtifactError not invoked: list=%q err=%v", gotList, gotErr)
	}

	// Same non-fatality for the exact-mode artifact.
	gotList, gotErr = "", nil
	eb := exact.NewBuilder()
	eb.Add(0, []string{"marcus chen"})
	eidx, err := eb.Finish()
	if err != nil {
		t.Fatal(err)
	}
	st.saveArtifact(l, lp, nil, eidx)
	if gotErr == nil || gotList != "people" {
		t.Fatalf("OnArtifactError not invoked for exact save: list=%q err=%v", gotList, gotErr)
	}

	// Nil hook must also be safe.
	st.OnArtifactError = nil
	st.saveArtifact(l, lp, idx, nil)
	st.saveArtifact(l, lp, nil, eidx)
}

// TestStorePerListLocking: holding one list's mutation lock (as a long
// Compact would) must not block Store.List/Lists or mutations of OTHER
// lists; mutations of the SAME list must still serialize behind it.
func TestStorePerListLocking(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b"} {
		if _, err := st.CreateList(n, internalPersonCfg()); err != nil {
			t.Fatal(err)
		}
	}

	st.mu.RLock()
	lsB := st.lists["b"]
	st.mu.RUnlock()
	lsB.lock.Lock() // simulate a long-running Compact of "b"

	free := make(chan struct{})
	go func() {
		defer close(free)
		if _, ok := st.List("b"); !ok { // lookup of the locked list itself
			t.Error("List(b) lost the list")
		}
		if _, ok := st.List("a"); !ok {
			t.Error("List(a) lost the list")
		}
		if got := len(st.Lists()); got != 2 {
			t.Errorf("Lists() = %d lists, want 2", got)
		}
		if err := st.Upsert("a", []Entry{{ID: "x", Keys: []string{"Marcus Chen"}}}); err != nil {
			t.Errorf("Upsert(a): %v", err)
		}
	}()
	select {
	case <-free:
	case <-time.After(5 * time.Second):
		t.Fatal("lookups/other-list mutation blocked by one list's mutation lock")
	}

	// Same-list mutation must wait for the lock (journal order == memory order).
	blocked := make(chan error, 1)
	go func() { blocked <- st.Upsert("b", []Entry{{ID: "y", Keys: []string{"Dana Kovak"}}}) }()
	select {
	case <-blocked:
		t.Fatal("Upsert(b) did not serialize behind b's mutation lock")
	case <-time.After(50 * time.Millisecond):
	}
	lsB.lock.Unlock()
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("Upsert(b) after unlock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Upsert(b) still blocked after unlock")
	}
}
