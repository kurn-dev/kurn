package exact_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/kurn-dev/kurn/engine/exact"
)

// decompose flattens an index into Restore's canonical inputs (sorted-key
// order), mirroring what the artifact loader reconstructs from disk.
func decompose(idx *exact.Index) (arena []byte, keyLens, runLens []int, postings []uint32) {
	for _, k := range idx.SortedKeys() {
		run := idx.Lookup(k)
		arena = append(arena, k...)
		keyLens = append(keyLens, len(k))
		runLens = append(runLens, len(run))
		postings = append(postings, run...)
	}
	return
}

func TestSortedKeys(t *testing.T) {
	b := exact.NewBuilder()
	b.Add(0, []string{"zeta", "alpha"})
	b.Add(1, []string{"mid", "alpha"})
	idx := mustFinish(t, b)
	got := idx.SortedKeys()
	want := []string{"alpha", "mid", "zeta"}
	if !slices.Equal(got, want) {
		t.Fatalf("SortedKeys() = %v, want %v", got, want)
	}
}

func TestNumOrds(t *testing.T) {
	if n := mustFinish(t, exact.NewBuilder()).NumOrds(); n != 0 {
		t.Fatalf("empty index NumOrds = %d, want 0", n)
	}
	b := exact.NewBuilder()
	b.Add(3, []string{"a"})
	b.Add(7, []string{"b", "a"})
	if n := mustFinish(t, b).NumOrds(); n != 8 {
		t.Fatalf("NumOrds = %d, want 8 (max ord 7 + 1)", n)
	}
}

// TestRestoreRoundTrip: Restore over a decomposed index must answer every
// Lookup exactly like the original, and carry the same NumOrds.
func TestRestoreRoundTrip(t *testing.T) {
	b := exact.NewBuilder()
	b.Add(0, []string{"bad@example.com"})
	b.Add(1, []string{"worse@example.com", "alias@example.com"})
	b.Add(2, []string{"bad@example.com"})
	idx := mustFinish(t, b)
	restored, err := exact.Restore(decompose(idx))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"bad@example.com", "worse@example.com", "alias@example.com", "", "miss"} {
		if got, want := restored.Lookup(q), idx.Lookup(q); !reflect.DeepEqual(got, want) {
			t.Errorf("Lookup(%q) = %v, want %v", q, got, want)
		}
	}
	if got, want := restored.NumOrds(), idx.NumOrds(); got != want {
		t.Errorf("NumOrds = %d, want %d", got, want)
	}
	if got, want := restored.Keys(), idx.Keys(); got != want {
		t.Errorf("Keys = %d, want %d", got, want)
	}
	// Empty index round-trips too.
	if empty, err := exact.Restore(nil, nil, nil, nil); err != nil || empty.Keys() != 0 || empty.NumOrds() != 0 {
		t.Errorf("empty Restore: idx=%+v err=%v", empty, err)
	}
}

// TestRestoreRejects: structurally inconsistent inputs (the kind a corrupt or
// mismatched artifact would produce) must error, never panic or build a
// broken index.
func TestRestoreRejects(t *testing.T) {
	cases := []struct {
		name     string
		arena    string
		keyLens  []int
		runLens  []int
		postings []uint32
	}{
		{"lens length mismatch", "ab", []int{1, 1}, []int{2}, []uint32{0, 1}},
		{"zero keyLen", "ab", []int{0, 2}, []int{1, 1}, []uint32{0, 1}},
		{"keyLen past arena", "ab", []int{1, 2}, []int{1, 1}, []uint32{0, 1}},
		{"arena not fully consumed", "abc", []int{1, 1}, []int{1, 1}, []uint32{0, 1}},
		{"zero runLen", "ab", []int{1, 1}, []int{0, 2}, []uint32{0, 1}},
		{"runLen past postings", "ab", []int{1, 1}, []int{1, 2}, []uint32{0, 1}},
		{"postings not fully consumed", "ab", []int{1, 1}, []int{1, 1}, []uint32{0, 1, 2}},
		{"duplicate key", "aa", []int{1, 1}, []int{1, 1}, []uint32{0, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exact.Restore([]byte(tc.arena), tc.keyLens, tc.runLens, tc.postings); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
	// A run over the 2^20-1 packed-length cap must be rejected, not silently
	// truncated by the bitfield encoding.
	big := make([]uint32, 1<<20)
	if _, err := exact.Restore([]byte("a"), []int{1}, []int{len(big)}, big); err == nil {
		t.Fatal("runLen over the packed cap accepted")
	}
	// Ordinal ^uint32(0) must be rejected: its +1 would overflow NumOrds to 0,
	// defeating the install path's max-ordinal-vs-entry-count validation.
	if _, err := exact.Restore([]byte("a"), []int{1}, []int{1}, []uint32{^uint32(0)}); err == nil {
		t.Fatal("ordinal MaxUint32 accepted")
	}
}
