//go:build (!amd64 && !386 && !arm && !arm64 && !ppc64le && !mipsle && !mips64le && !mips64p32le && !wasm) || appengine

package prepared

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/RoaringBitmap/roaring"
)

// On this architecture roaring is built without its frozen bitmap format
// (its serialization_generic.go, the complement of the constraint in
// frozen_roaring.go), so n-gram sections can be neither written nor read:
// Encode and NewView refuse any artifact with an ngram section with
// errNoFrozen.
var errNoFrozen = fmt.Errorf("prepared: this architecture's roaring build (%s) has no frozen bitmap format, so n-gram artifacts cannot be written or read: %w", runtime.GOARCH, errors.ErrUnsupported)

func frozenSupported() error { return errNoFrozen }

func frozenSize(*roaring.Bitmap) (uint64, error) { return 0, errNoFrozen }

func freezeTo(*roaring.Bitmap, []byte) (int, error) { return 0, errNoFrozen }

func frozenView(*roaring.Bitmap, []byte) error { return errNoFrozen }
