package ngram

import "testing"

// Admission control charges exactly 4 B × numOrds for the touched slice, so
// its capacity must be exactly numOrds — preallocated, never grown. The
// previous grown-by-append slice retained ~8-11% more capacity than the
// model on flood queries (110,592 for 100,000 ordinals), memory the
// governor never admitted. Inspecting a scratch after putScratch is valid
// only because this single-goroutine test has no intervening getScratch.
func TestScratchCapacityMatchesTheChargedModel(t *testing.T) {
	const n = 1000
	b := NewBuilder(Config{Grams: []int{2}})
	for i := 0; i < n; i++ {
		// Every key shares the gram "zq": one posting floods all ordinals.
		b.Add(uint32(i), []string{"zq"})
	}
	idx := b.Finish()
	if idx.numOrds != n {
		t.Fatalf("numOrds = %d, want %d", idx.numOrds, n)
	}

	s := idx.getScratch()
	if c := cap(s.touched); c != n {
		t.Fatalf("fresh scratch touched capacity %d, want exactly %d", c, n)
	}
	if c := len(s.counts); c != n {
		t.Fatalf("fresh scratch counts length %d, want %d", c, n)
	}
	idx.putScratch(s)

	// A flood lookup touches every ordinal; the pooled slice must come back
	// reset and STILL at exactly the charged capacity.
	if hits := idx.Lookup("zq", 0, 1); len(hits) != 1 {
		t.Fatalf("flood lookup returned %d hits, want 1", len(hits))
	}
	s2 := idx.getScratch()
	if c := cap(s2.touched); c != n {
		t.Fatalf("post-flood touched capacity %d, want exactly %d — the model was overshot or trimmed", c, n)
	}
	if len(s2.touched) != 0 {
		t.Fatalf("touched len %d after putScratch, want 0", len(s2.touched))
	}
	idx.putScratch(s2)
}
