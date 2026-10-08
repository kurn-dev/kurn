//go:build unix && ((386 && !appengine) || (amd64 && !appengine) || (arm && !appengine) || (arm64 && !appengine) || (ppc64le && !appengine) || (mipsle && !appengine) || (mips64le && !appengine) || (mips64p32le && !appengine) || (wasm && !appengine))

package prepared

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"
)

// TestFrozenViewOverMappingAtEveryWriterAlignment is the mapped
// equivalent of TestFrozenViewAtEveryWriterAlignment: every embedded
// bitmap is loaded through FrozenView from a read-only file mapping
// (which starts on a page boundary) and compared with the index it was
// frozen from. A padded build section shifts the index by the same page
// offsets the heap test uses, so bitmap starts land in every 32-byte slot
// of a page. Run it under -race, which enables Go's pointer checks for
// unsafe conversions. The mappings bypass the registry and are unmapped
// at the end: they are never exposed.
func TestFrozenViewOverMappingAtEveryWriterAlignment(t *testing.T) {
	idx := alignmentIndex()
	recs := make([]Record, idx.NumOrds())
	for i := range recs {
		recs[i] = Record{ID: fmt.Sprintf("r%06d", i), Keys: []string{"k"}}
	}
	dir := t.TempDir()
	slots := map[uintptr]bool{}
	page := uintptr(os.Getpagesize())
	for _, shift := range []int{0, 32, 64, 96, 2048, 4064} {
		c := &Contents{
			Name: "align", AnalyzerDigest: fakeAnalyzerDigest, ConfigDigest: fakeConfigDigest, Records: recs,
			ListConfig: []byte(`{"analyzer":{"steps":["lowercase"]},"match":{"mode":"ngram","grams":[2,3],"threshold":0.6,"topk":100}}`),
			Ngram:      idx, Source: "s" + strings.Repeat("p", shift),
		}
		buf, _ := encodeT(t, c)
		path := filepath.Join(dir, fmt.Sprintf("a%d.kart", shift))
		if err := os.WriteFile(path, buf, 0o444); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		m, err := mapFile(f, len(buf))
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if uintptr(unsafe.Pointer(unsafe.SliceData(m)))%page != 0 || !isAligned(m) {
			t.Fatalf("mapping at %p is not page-aligned", unsafe.SliceData(m))
		}
		v, _, err := VerifyBytes(m, LevelFull)
		if err != nil {
			t.Fatalf("shift %d: %v", shift, err)
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
				t.Fatalf("gram %q differs after FrozenView over the mapping", v.Gram(i))
			}
			it := bm.ManyIterator()
			tmp := make([]uint32, 64)
			var n uint64
			for k := it.NextMany(tmp); k > 0; k = it.NextMany(tmp) {
				n += uint64(k)
			}
			if n != want.GetCardinality() || !bm.Contains(want.Minimum()) {
				t.Fatalf("gram %q: iteration or Contains disagrees", v.Gram(i))
			}
		}
		if _, err := v.NgramIndex(); err != nil {
			t.Fatal(err)
		}
		v = nil
		if err := unmapFile(m); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("bitmaps over mappings covered %d of the 128 32-byte slots of a 4 KiB page", len(slots))
	if len(slots) < 128 {
		t.Fatalf("bitmaps landed in only %d of the 128 32-byte slots of a page", len(slots))
	}
}
