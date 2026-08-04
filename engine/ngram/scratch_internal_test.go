package ngram

import "testing"

// putScratch must shed a worst-case touched slice (flood query) instead of
// pinning it in the pool forever, but keep a well-sized one. Inspecting a
// scratch after putScratch is valid only because this single-goroutine test
// has no intervening getScratch that could hand it out to another user.
func TestPutScratchTrimsOversizedTouched(t *testing.T) {
	b := NewBuilder(Config{Grams: []int{2}})
	b.Add(0, []string{"elena vasquez"})
	b.Add(1, []string{"marcus chen"})
	idx := b.Finish()

	// Flood-shaped: huge cap, tiny use this time -> reallocated smaller.
	s := idx.getScratch()
	s.touched = append(make([]uint32, 0, scratchTrimCap+1), 0, 1)
	idx.putScratch(s)
	if c := cap(s.touched); c > 2 {
		t.Errorf("oversized touched kept cap %d, want <= 2 (trimmed to used)", c)
	}
	if len(s.touched) != 0 {
		t.Errorf("touched len %d after putScratch, want 0", len(s.touched))
	}

	// Well-used: huge cap but used > cap/4 -> kept (no realloc thrash).
	s2 := idx.getScratch()
	big := make([]uint32, 0, scratchTrimCap+1)
	for i := 0; i < (scratchTrimCap+1)/2; i++ {
		big = append(big, uint32(i%2)) // only valid ords; counts has numOrds=2
	}
	s2.touched = big
	idx.putScratch(s2)
	if c := cap(s2.touched); c != scratchTrimCap+1 {
		t.Errorf("well-used touched cap %d, want %d (kept)", c, scratchTrimCap+1)
	}
}
