//go:build (386 && !appengine) || (amd64 && !appengine) || (arm && !appengine) || (arm64 && !appengine) || (ppc64le && !appengine) || (mipsle && !appengine) || (mips64le && !appengine) || (mips64p32le && !appengine) || (wasm && !appengine)

package prepared

import (
	"encoding/binary"
	"errors"
	"math/rand"
	"testing"

	"github.com/RoaringBitmap/roaring"
)

// frozenFixtures are bitmaps covering every container type roaring can
// freeze, alone and mixed across containers.
func frozenFixtures() map[string]*roaring.Bitmap {
	rng := rand.New(rand.NewSource(5))
	arr := roaring.BitmapOf(3, 9, 70000, 1<<20)
	bitset := roaring.New()
	for i := 0; i < 9000; i++ {
		bitset.Add(uint32(rng.Intn(60000)))
	}
	run := roaring.New()
	run.AddRange(100, 20000)
	run.AddRange(65536*3, 65536*3+500)
	run.RunOptimize()
	mixed := roaring.New()
	mixed.Or(arr)
	mixed.Or(bitset)
	mixed.AddRange(65536*7+10, 65536*7+40000)
	mixed.RunOptimize()
	full := roaring.New()
	full.AddRange(0, 65536) // one full run container: last = 0xffff
	full.RunOptimize()
	return map[string]*roaring.Bitmap{
		"single": roaring.BitmapOf(42), "array": arr, "bitset": bitset,
		"run": run, "mixed": mixed, "full": full, "max": roaring.BitmapOf(0, ^uint32(0)),
	}
}

func freeze(t *testing.T, bm *roaring.Bitmap) []byte {
	t.Helper()
	b := AlignedBuffer(int(bm.GetFrozenSizeInBytes()))
	if _, err := bm.FreezeTo(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCheckFrozenAcceptsRoaringOutput(t *testing.T) {
	for name, bm := range frozenFixtures() {
		b := freeze(t, bm)
		fi, err := checkFrozen(b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fi.cardinality != bm.GetCardinality() || fi.max != bm.Maximum() {
			t.Fatalf("%s: check says card %d max %d, roaring says %d, %d", name, fi.cardinality, fi.max, bm.GetCardinality(), bm.Maximum())
		}
		v := roaring.New()
		if err := v.FrozenView(b); err != nil || !v.Equals(bm) {
			t.Fatalf("%s: frozen view %v", name, err)
		}
	}
}

// frozenBytes hand-assembles a frozen bitmap.
func frozenBytes(data []byte, keys, counts []uint16, types []byte) []byte {
	n := len(types)
	b := AlignedBuffer(len(data) + 5*n + 4)
	copy(b, data)
	at := len(data)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(b[at+2*i:], keys[i])
		binary.LittleEndian.PutUint16(b[at+2*n+2*i:], counts[i])
		b[at+4*n+i] = types[i]
	}
	binary.LittleEndian.PutUint32(b[at+5*n:], uint32(frozenCookie|n<<15))
	return b
}

func u16s(v ...uint16) []byte {
	b := make([]byte, 2*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint16(b[2*i:], x)
	}
	return b
}

func TestCheckFrozenRefuses(t *testing.T) {
	bitset := make([]byte, frozenBitsetBytes)
	bitset[0] = 0xff // 8 values; a count of 4999 disagrees
	cases := map[string][]byte{
		"short":                 {1, 2, 3},
		"empty run, consistent": frozenBytes(nil, []uint16{0}, []uint16{0}, []byte{3}),
		"keys not increasing":   frozenBytes(u16s(1, 2), []uint16{5, 5}, []uint16{0, 0}, []byte{2, 2}),
		"keys descending":       frozenBytes(u16s(1, 2), []uint16{6, 5}, []uint16{0, 0}, []byte{2, 2}),
		"array unsorted":        frozenBytes(u16s(9, 3), []uint16{0}, []uint16{1}, []byte{2}),
		"array duplicate":       frozenBytes(u16s(3, 3), []uint16{0}, []uint16{1}, []byte{2}),
		"array over 4096":       frozenBytes(make([]byte, 2*4097), []uint16{0}, []uint16{4096}, []byte{2}),
		"bitset popcount":       frozenBytes(bitset, []uint16{0}, []uint16{4999}, []byte{1}),
		"run overflows":         frozenBytes(u16s(0xfff0, 0x20), []uint16{0}, []uint16{1}, []byte{3}),
		"runs overlap":          frozenBytes(u16s(10, 5, 12, 1), []uint16{0}, []uint16{2}, []byte{3}),
		"runs descending":       frozenBytes(u16s(50, 1, 10, 1), []uint16{0}, []uint16{2}, []byte{3}),
		"typecode zero":         frozenBytes(u16s(1), []uint16{0}, []uint16{0}, []byte{0}),
		"data too long":         frozenBytes(u16s(1, 2), []uint16{0}, []uint16{0}, []byte{2}),
	}
	be := frozenBytes(u16s(1), []uint16{0}, []uint16{0}, []byte{2})
	binary.BigEndian.PutUint32(be[len(be)-4:], uint32(frozenCookie|1<<15))
	cases["big-endian header"] = be
	for name, b := range cases {
		if _, err := checkFrozen(b); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: got %v, want ErrMalformed", name, err)
		}
	}
	// And the well-formed hand-built ones pass, so the refusals above are
	// about their defects, not about the assembler.
	for name, b := range map[string][]byte{
		"array":  frozenBytes(u16s(3, 9), []uint16{0}, []uint16{1}, []byte{2}),
		"runs":   frozenBytes(u16s(10, 5, 16, 1), []uint16{0}, []uint16{2}, []byte{3}),
		"two":    frozenBytes(u16s(1, 2), []uint16{5, 6}, []uint16{0, 0}, []byte{2, 2}),
		"bitset": frozenBytes(bitset, []uint16{0}, []uint16{7}, []byte{1}),
	} {
		if _, err := checkFrozen(b); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
