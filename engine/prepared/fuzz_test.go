//go:build (386 && !appengine) || (amd64 && !appengine) || (arm && !appengine) || (arm64 && !appengine) || (ppc64le && !appengine) || (mipsle && !appengine) || (mips64le && !appengine) || (mips64p32le && !appengine) || (wasm && !appengine)

package prepared

import (
	"testing"

	"github.com/RoaringBitmap/roaring"
)

// exercise drives every accessor of an accepted View, including the index
// rebuilds and query-path bitmap operations. On accepted input none of it
// may panic.
func exercise(t *testing.T, v *View) {
	for i := 0; i < v.Len(); i++ {
		id := v.ID(i)
		v.Keys(i)
		v.Payload(i)
		if o, ok := v.Lookup(id); !ok || o != i {
			t.Fatalf("Lookup(%q) = %d, %v; want %d", id, o, ok, i)
		}
	}
	v.Config()
	v.Build()
	v.NumOrds()
	if v.GramCount() > 0 || v.ex == nil {
		for i := 0; i < v.GramCount(); i++ {
			bm, err := v.Postings(i)
			if err != nil {
				t.Fatalf("accepted view refused bitmap %d: %v", i, err)
			}
			exerciseBitmap(bm)
		}
		idx, err := v.NgramIndex()
		if err != nil {
			t.Fatalf("accepted view refused its ngram index: %v", err)
		}
		for i := 0; i < v.GramCount() && i < 8; i++ {
			idx.Lookup(v.Gram(i), 0, 10)
		}
		idx.Lookup("anna smith", 0.2, 5)
		return
	}
	idx, err := v.ExactIndex()
	if err != nil {
		t.Fatalf("accepted view refused its exact index: %v", err)
	}
	for i := 0; i < v.Len() && i < 8; i++ {
		for _, k := range v.Keys(i) {
			idx.Lookup(normalize(k))
		}
	}
}

func exerciseBitmap(bm *roaring.Bitmap) {
	bm.GetCardinality()
	if !bm.IsEmpty() {
		bm.Maximum()
		bm.Minimum()
	}
	it := bm.ManyIterator()
	buf := make([]uint32, 256)
	for n := it.NextMany(buf); n > 0; n = it.NextMany(buf) {
		bm.Contains(buf[n-1])
	}
	bm.Contains(0)
	bm.Contains(1 << 31)
}

func seedArtifacts(f *testing.F) [][]byte {
	var out [][]byte
	for _, mode := range []string{"ngram", "exact"} {
		for _, n := range []int{1, 3, 17} {
			out = append(out, artifactT(f, mode, n))
		}
	}
	return out
}

// FuzzNewView feeds arbitrary bytes to the parser, then (rehashed so the
// digests pass) to full verification.
func FuzzNewView(f *testing.F) {
	for _, a := range seedArtifacts(f) {
		f.Add(a)
		f.Add(a[:len(a)/2])
	}
	for _, mode := range []string{"ngram", "exact"} {
		f.Add(invalidUTF8Artifact(f, mode, false)) // F1: non-UTF-8 ID, rehashed
		f.Add(invalidUTF8Artifact(f, mode, true))  // F1: non-UTF-8 key, rehashed
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		b := clone(data)
		if v, err := NewView(b); err == nil {
			exercise(t, v)
		}
		rehash(b)
		if v, _, err := VerifyBytes(b, LevelFull); err == nil {
			exercise(t, v)
		}
	})
}

// FuzzArtifactMutation XORs fuzz bytes into a valid artifact at a fuzz
// position and recomputes every digest, so the deep structural checks —
// not the hashes — are what stand between the bytes and the accessors.
func FuzzArtifactMutation(f *testing.F) {
	bases := seedArtifacts(f)
	for i := range bases {
		// F1: turn the first byte of record 0's ID into a lone UTF-8
		// leading byte (ASCII ^ 0x80), digests recomputed by the target.
		v, err := NewView(bases[i])
		if err != nil {
			f.Fatal(err)
		}
		f.Add(uint8(i), uint32(offsetIn(bases[i], v.ID(0))), []byte{0x80})
		f.Add(uint8(i), uint32(0), []byte{1})
		f.Add(uint8(i), uint32(len(bases[i])/2), []byte{0xff, 0x00, 0x80})
		f.Add(uint8(i), uint32(len(bases[i])-5), []byte{0x7f})
	}
	f.Fuzz(func(t *testing.T, which uint8, pos uint32, x []byte) {
		base := bases[int(which)%len(bases)]
		b := clone(base)
		for i, c := range x {
			b[(int(pos)+i)%len(b)] ^= c
		}
		rehash(b)
		if v, _, err := VerifyBytes(b, LevelFull); err == nil {
			exercise(t, v)
		}
		if v, err := NewView(b); err == nil {
			exercise(t, v)
		}
	})
}

// FuzzCheckFrozen checks this package's frozen-trailer validation against
// roaring's FrozenView: FrozenView itself must not panic on any aligned
// input, and whenever checkFrozen accepts, the view must succeed, agree on
// cardinality and maximum, and survive the query-path operations.
func FuzzCheckFrozen(f *testing.F) {
	for _, bm := range frozenFixtures() {
		b := AlignedBuffer(int(bm.GetFrozenSizeInBytes()))
		bm.FreezeTo(b)
		f.Add(b)
	}
	f.Add(frozenBytes(u16s(1, 2), []uint16{5, 6}, []uint16{0, 0}, []byte{2, 2}))
	f.Add(frozenBytes(u16s(10, 5, 16, 1), []uint16{0}, []uint16{2}, []byte{3}))
	f.Fuzz(func(t *testing.T, data []byte) {
		b := clone(data)
		// Roaring's parser on arbitrary aligned input: errors are fine,
		// panics are findings.
		roaring.New().FrozenView(b)
		fi, err := checkFrozen(b)
		if err != nil {
			return
		}
		bm := roaring.New()
		if err := bm.FrozenView(b); err != nil {
			t.Fatalf("checkFrozen accepted what FrozenView refuses: %v", err)
		}
		if bm.GetCardinality() != fi.cardinality || bm.Maximum() != fi.max {
			t.Fatalf("roaring says card %d max %d; check says %d, %d", bm.GetCardinality(), bm.Maximum(), fi.cardinality, fi.max)
		}
		exerciseBitmap(bm)
		var n uint64
		it := bm.Iterator()
		for it.HasNext() {
			if v := it.Next(); !bm.Contains(v) {
				t.Fatalf("iterated %d but Contains says no", v)
			}
			n++
		}
		if n != fi.cardinality {
			t.Fatalf("iterated %d values, cardinality %d", n, fi.cardinality)
		}
	})
}
