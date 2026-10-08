//go:build (386 && !appengine) || (amd64 && !appengine) || (arm && !appengine) || (arm64 && !appengine) || (ppc64le && !appengine) || (mipsle && !appengine) || (mips64le && !appengine) || (mips64p32le && !appengine) || (wasm && !appengine)

package prepared

import (
	"fmt"
	"testing"
	"unsafe"

	"github.com/RoaringBitmap/roaring"
	"github.com/kurn-dev/kurn/engine/ngram"
)

// TestFrozenViewAtEveryWriterAlignment loads every embedded bitmap through
// FrozenView and compares it with the index it was frozen from. Grams of
// varied lengths and bitmaps of varied frozen sizes (array, bitset and run
// containers; 9 + 2k bytes for small arrays) put bitmap starts at many
// distinct 32-byte slots, and the artifact is placed at several page
// offsets. Run it under -race: the race build enables Go's pointer checks
// for unsafe conversions.
func TestFrozenViewAtEveryWriterAlignment(t *testing.T) {
	idx := alignmentIndex()
	recs := make([]Record, idx.NumOrds())
	for i := range recs {
		recs[i] = Record{ID: fmt.Sprintf("r%06d", i), Keys: []string{"k"}}
	}
	c := &Contents{
		Name: "align", AnalyzerDigest: fakeAnalyzerDigest, ConfigDigest: fakeConfigDigest, Records: recs,
		ListConfig: []byte(`{"analyzer":{"steps":["lowercase"]},"match":{"mode":"ngram","grams":[2,3],"threshold":0.6,"topk":100}}`),
		Ngram:      idx,
	}
	buf, _ := encodeT(t, c)
	slots := map[uintptr]bool{}
	for _, pageOff := range []int{0, 32, 64, 96, 2048, 4064} {
		b := AlignedBuffer(len(buf) + pageOff + 4096)
		// Slice to the wanted offset within a 4 KiB page.
		base := int(uintptr(unsafe.Pointer(&b[0])) % 4096)
		start := (4096 - base + pageOff) % 4096
		a := b[start : start+len(buf) : start+len(buf)]
		copy(a, buf)
		v, _, err := VerifyBytes(a, LevelFull)
		if err != nil {
			t.Fatalf("page offset %d: %v", pageOff, err)
		}
		post := idx.Postings()
		for i := 0; i < v.GramCount(); i++ {
			fb := v.FrozenBitmap(i)
			addr := uintptr(unsafe.Pointer(&fb[0]))
			if addr%Align != 0 {
				t.Fatalf("bitmap %d at %#x is not aligned", i, addr)
			}
			slots[(addr%4096)/Align] = true
			bm, err := v.Postings(i)
			if err != nil {
				t.Fatal(err)
			}
			want := post[v.Gram(i)]
			if !bm.Equals(want) || bm.GetCardinality() != want.GetCardinality() || bm.Maximum() != want.Maximum() {
				t.Fatalf("gram %q differs after FrozenView", v.Gram(i))
			}
			// Exercise the query-path operations on the view.
			it := bm.ManyIterator()
			buf := make([]uint32, 64)
			var n uint64
			for k := it.NextMany(buf); k > 0; k = it.NextMany(buf) {
				n += uint64(k)
			}
			if n != want.GetCardinality() || !bm.Contains(want.Minimum()) {
				t.Fatalf("gram %q: iteration or Contains disagrees", v.Gram(i))
			}
		}
	}
	t.Logf("bitmaps covered %d of the 128 32-byte slots of a 4 KiB page", len(slots))
	if len(slots) < 128 {
		t.Fatalf("bitmaps landed in only %d of the 128 32-byte slots of a page; the fixture does not cover the alignments", len(slots))
	}
}

// alignmentIndex builds an ngram index whose postings mix small arrays of
// many sizes, bitset containers and run containers.
func alignmentIndex() *ngram.Index {
	post := map[string]*roaring.Bitmap{}
	const ords = 70000
	for i := 0; i < 400; i++ {
		bm := roaring.New()
		switch {
		case i%37 == 0: // bitset container
			for o := 0; o < 5000; o++ {
				bm.Add(uint32((o*13 + i) % 60000))
			}
		case i%41 == 0: // run containers, one crossing into the next key
			bm.AddRange(uint64(i), uint64(i+66000))
			bm.RunOptimize()
		default: // array of i%23+1 values
			for o := 0; o <= i%23; o++ {
				bm.Add(uint32(o*97 + i))
			}
		}
		post[fmt.Sprintf("g%0*d", 1+i%7, i)] = bm
	}
	post["top"] = roaring.BitmapOf(ords - 1)
	idx, err := ngram.Restore(ngram.Config{Grams: []int{2, 3}}, post, ords)
	if err != nil {
		panic(err)
	}
	return idx
}
