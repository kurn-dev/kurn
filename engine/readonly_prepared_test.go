package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

// prepMutators returns every exported Store mutator (14) and every
// exported List mutator with an error return (9), each as a call that
// returns its error, plus the two List no-ops (SetBaseID, CommitCompact).
func prepMutators(st *engine.Store, l *engine.List, name string) (errCalls map[string]func() error, noops map[string]func()) {
	es := []engine.Entry{{ID: "new", Keys: []string{"anna smith"}, Payload: json.RawMessage(`{"program":"SDN"}`)}}
	cfg := engine.ListConfig{Match: engine.MatchConfig{Mode: "exact"}}
	errCalls = map[string]func() error{
		"Store.CreateList":          func() error { _, err := st.CreateList(name, cfg); return err },
		"Store.CreateListVersioned": func() error { _, _, err := st.CreateListVersioned(name, cfg); return err },
		"Store.Upsert":              func() error { return st.Upsert(name, es) },
		"Store.UpsertVersioned":     func() error { _, err := st.UpsertVersioned(name, es); return err },
		"Store.UpsertGen":           func() error { return st.UpsertGen(name, l, es) },
		"Store.UpsertGenVersioned":  func() error { _, err := st.UpsertGenVersioned(name, l, es); return err },
		"Store.Delete":              func() error { return st.Delete(name, "p-00000") },
		"Store.DeleteVersioned":     func() error { _, err := st.DeleteVersioned(name, "p-00000"); return err },
		"Store.ReloadList":          func() error { _, err := st.ReloadList(name); return err },
		"Store.ReloadListVersioned": func() error { _, _, err := st.ReloadListVersioned(name); return err },
		"Store.Compact":             func() error { return st.Compact(name) },
		"Store.CompactVersioned":    func() error { _, err := st.CompactVersioned(name); return err },
		"Store.Replace":             func() error { return st.Replace(name, es) },
		"Store.ReplaceVersioned":    func() error { _, err := st.ReplaceVersioned(name, es); return err },

		"List.Replace":                   func() error { return l.Replace(es) },
		"List.ReplaceWithIndex":          func() error { return l.ReplaceWithIndex(es, nil) },
		"List.ReplaceWithIndexInfo":      func() error { return l.ReplaceWithIndexInfo(es, nil, nil) },
		"List.ReplaceWithExactIndex":     func() error { return l.ReplaceWithExactIndex(es, nil) },
		"List.ReplaceWithExactIndexInfo": func() error { return l.ReplaceWithExactIndexInfo(es, nil, nil) },
		"List.PrepareCompact":            func() error { _, err := l.PrepareCompact(); return err },
		"List.Upsert":                    func() error { return l.Upsert(es) },
		"List.Delete":                    func() error { return l.Delete("p-00000") },
		"List.Compact":                   func() error { return l.Compact() },
	}
	noops = map[string]func(){
		"List.SetBaseID":     func() { l.SetBaseID("0000") },
		"List.CommitCompact": func() { l.CommitCompact(nil, "0000") },
	}
	return errCalls, noops
}

// (b) Every mutator is refused (or a counted no-op) before touching any
// state: answers and stamp are unchanged, every refusal is counted, and
// nothing in the artifact's directory is created or changed.
func TestPreparedReadOnly(t *testing.T) {
	for _, c := range prepCases() {
		t.Run(c.name, func(t *testing.T) {
			path := prepWriteArtifact(t, c)
			dir := filepath.Dir(path)
			before := prepSnapshotDir(t, dir)
			st, err := engine.OpenPrepared(path, engine.PreparedOptions{})
			if err != nil {
				t.Fatal(err)
			}
			l := prepList(t, st)
			want := prepAnswers(t, l, c)
			stamp := l.Version()

			var expect uint64
			run := func(names ...string) {
				errCalls, noops := prepMutators(st, l, names[0])
				if len(errCalls) != 23 || len(noops) != 2 {
					t.Fatalf("mutator table has %d+%d entries", len(errCalls), len(noops))
				}
				for _, name := range names {
					errCalls, noops = prepMutators(st, l, name)
					for what, call := range errCalls {
						if err := call(); !errors.Is(err, engine.ErrReadOnly) {
							t.Errorf("%s(%q): %v, want ErrReadOnly", what, name, err)
						}
						expect++
					}
					for _, call := range noops {
						call()
						expect++
					}
				}
			}
			// The served list, an unknown list, and an invalid name.
			run(prepListName, "other", "Bad Name!")
			if got := st.ReadOnlyRefusals(); got != expect {
				t.Fatalf("ReadOnlyRefusals %d, want %d", got, expect)
			}
			if got := prepAnswers(t, l, c); !bytes.Equal(got, want) {
				t.Fatalf("answers changed:\n%s", prepFirstDiff(got, want))
			}
			if l.Version() != stamp || len(st.Lists()) != 1 || len(st.DamagedLists()) != 0 {
				t.Fatalf("state changed: version %s (was %s), lists %d, damaged %v", l.Version(), stamp, len(st.Lists()), st.DamagedLists())
			}
			// Close keeps its meaning, reads keep working, and mutations
			// stay refused (and counted).
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			run(prepListName)
			if got := st.ReadOnlyRefusals(); got != expect {
				t.Fatalf("after Close: ReadOnlyRefusals %d, want %d", got, expect)
			}
			if got := prepAnswers(t, prepList(t, st), c); !bytes.Equal(got, want) {
				t.Fatal("answers changed after Close")
			}
			after := prepSnapshotDir(t, dir)
			if fmt.Sprint(after) != fmt.Sprint(before) {
				t.Fatalf("artifact directory changed:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// (g) Concurrent queries, refused mutators and a Close, under -race: every
// answer stays the baseline and every refusal is counted.
func TestPreparedConcurrentReadOnly(t *testing.T) {
	c := prepCases()[2]
	path := prepWriteArtifact(t, c)
	st, err := engine.OpenPrepared(path, engine.PreparedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	l := prepList(t, st)
	ctx := context.Background()
	filter := map[string]string{"program": "SDN"}
	answer := func(ll *engine.List, q string) string {
		cs, ver := ll.QueryVersioned(ctx, q, engine.QueryOpts{})
		fcs, fver, ferr := ll.QueryFilteredCtx(ctx, q, engine.QueryOpts{}, filter)
		b, _ := json.Marshal([]any{cs, ver, fcs, fver, fmt.Sprint(ferr)})
		return string(b)
	}
	baseline := map[string]string{}
	for _, q := range c.queries {
		baseline[q] = answer(l, q)
	}
	iters := 200
	if raceEnabled {
		iters = 40
	}
	var calls atomic.Uint64
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				q := c.queries[(g+i)%len(c.queries)]
				ll, ok := st.List(prepListName)
				if !ok {
					t.Error("list vanished")
					return
				}
				if answer(ll, q) != baseline[q] {
					t.Errorf("query %q changed under concurrency", q)
					return
				}
			}
		}(g)
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			errCalls, noops := prepMutators(st, l, prepListName)
			for i := 0; i < iters/4; i++ {
				for what, call := range errCalls {
					if err := call(); !errors.Is(err, engine.ErrReadOnly) {
						t.Errorf("%s: %v", what, err)
					}
					calls.Add(1)
				}
				for _, call := range noops {
					call()
					calls.Add(1)
				}
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	if got, want := st.ReadOnlyRefusals(), calls.Load(); got != want {
		t.Fatalf("ReadOnlyRefusals %d, calls %d", got, want)
	}
	if l.Version() != prepList(t, st).Version() {
		t.Fatal("stamp changed")
	}
}
