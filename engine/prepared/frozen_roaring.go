//go:build (386 && !appengine) || (amd64 && !appengine) || (arm && !appengine) || (arm64 && !appengine) || (ppc64le && !appengine) || (mipsle && !appengine) || (mips64le && !appengine) || (mips64p32le && !appengine) || (wasm && !appengine)

package prepared

import "github.com/RoaringBitmap/roaring"

// The frozen bitmap format (FreezeTo, FrozenView, GetFrozenSizeInBytes)
// exists in roaring only under the build constraint above, which is
// roaring's own (serialization_littleendian.go). Every use of it in this
// package goes through these wrappers; frozen_unsupported.go has the
// complement.

// frozenSupported reports whether this build has the frozen format.
func frozenSupported() error { return nil }

// frozenSize is bm.GetFrozenSizeInBytes.
func frozenSize(bm *roaring.Bitmap) (uint64, error) { return bm.GetFrozenSizeInBytes(), nil }

// freezeTo is bm.FreezeTo.
func freezeTo(bm *roaring.Bitmap, dst []byte) (int, error) { return bm.FreezeTo(dst) }

// frozenView is bm.FrozenView.
func frozenView(bm *roaring.Bitmap, b []byte) error { return bm.FrozenView(b) }
