//go:build (!amd64 && !386 && !arm && !arm64 && !ppc64le && !mipsle && !mips64le && !mips64p32le && !wasm) || appengine

package prepared

import (
	"errors"
	"testing"

	"github.com/RoaringBitmap/roaring"
)

// Without roaring's frozen bitmap format every frozen helper refuses with
// ErrUnsupported, and so does encoding an n-gram artifact. (A big-endian
// host is refused earlier, by checkHost, with ErrIncompatible.) Exact-mode
// artifacts need no frozen bitmaps and keep working on little-endian hosts.
func TestNoFrozenFormatRefuses(t *testing.T) {
	bm := roaring.BitmapOf(1, 2, 3)
	_, sizeErr := frozenSize(bm)
	_, freezeErr := freezeTo(bm, make([]byte, 64))
	for name, err := range map[string]error{
		"frozenSupported": frozenSupported(), "frozenSize": sizeErr, "freezeTo": freezeErr,
		"frozenView": frozenView(roaring.New(), AlignedBuffer(64)),
	} {
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("%s: %v, want ErrUnsupported", name, err)
		}
	}
	_, _, err := Encode(testContents("ngram", testRecords(20, 1)))
	want := errors.ErrUnsupported
	if !hostLittleEndian {
		want = ErrIncompatible
	}
	if !errors.Is(err, want) {
		t.Fatalf("Encode of an n-gram artifact: %v, want %v", err, want)
	}
	if hostLittleEndian {
		buf, _, err := Encode(testContents("exact", testRecords(20, 1)))
		if err != nil {
			t.Fatalf("exact artifact: %v", err)
		}
		if _, _, err := VerifyBytes(buf, LevelFull); err != nil {
			t.Fatalf("exact artifact: %v", err)
		}
	}
}
