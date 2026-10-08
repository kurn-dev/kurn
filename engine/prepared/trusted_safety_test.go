//go:build (386 && !appengine) || (amd64 && !appengine) || (arm && !appengine) || (arm64 && !appengine) || (ppc64le && !appengine) || (mipsle && !appengine) || (mips64le && !appengine) || (mips64p32le && !appengine) || (wasm && !appengine)

package prepared

import (
	"errors"
	"strings"
	"testing"
)

// Structural safety at LevelTrusted. Trusted skips exactly the content
// digests (records, ids, index), so an artifact whose content sections
// were changed arbitrarily reaches trusted's checks with every retained
// digest intact: that is what the rehashed suites below model. Only the
// structural checks stand between such bytes and the accessors.

// Every malformed-but-rehashed case is refused at LevelTrusted exactly as
// at LevelFull, and, not rehashed, is refused too.
func TestTrustedMalformedButRehashed(t *testing.T) {
	fixtures := map[string][]byte{"ngram": artifactT(t, "ngram", 400), "exact": artifactT(t, "exact", 400)}
	for _, c := range malformedCases() {
		t.Run(strings.ReplaceAll(c.name, " ", "_"), func(t *testing.T) {
			mode := c.mode
			if mode == "" {
				mode = "ngram"
			}
			b := c.mutate(t, fixtures[mode])
			if _, _, err := VerifyBytes(clone(b), LevelTrusted); err == nil {
				t.Fatal("trusted accepted the mutation without rehash")
			}
			if !c.raw {
				rehash(b)
			}
			want := c.want
			if want == nil {
				want = ErrMalformed
			}
			v, ver, err := VerifyBytes(b, LevelTrusted)
			if !errors.Is(err, want) || v != nil || ver != nil {
				t.Fatalf("trusted: got %v, want %v", err, want)
			}
		})
	}
}

// Every truncation fails at LevelTrusted, and every single-byte change,
// rehashed or not, is handled without panicking; whatever trusted accepts
// survives every accessor.
func TestTrustedEveryPrefixAndByteFlip(t *testing.T) {
	for _, mode := range []string{"ngram", "exact"} {
		buf := artifactT(t, mode, 12)
		for n := 0; n < len(buf); n++ {
			if _, _, err := VerifyBytes(clone(buf[:n]), LevelTrusted); err == nil {
				t.Fatalf("%s: %d-byte prefix verified at trusted", mode, n)
			}
		}
		accepted := 0
		for i := 0; i < len(buf); i++ {
			for _, x := range []byte{0x01, 0x80, 0xff} {
				b := clone(buf)
				b[i] ^= x
				for _, rh := range []bool{false, true} {
					c := clone(b)
					if rh {
						rehash(c)
					}
					if v, _, err := VerifyBytes(c, LevelTrusted); err == nil {
						exercise(t, v)
						accepted++
					}
				}
			}
		}
		t.Logf("%s: %d mutated variants accepted at trusted and exercised", mode, accepted)
	}
}

// FuzzTrustedMutation is FuzzArtifactMutation at LevelTrusted, both with
// every digest recomputed and with none: whatever trusted accepts must
// survive every accessor.
func FuzzTrustedMutation(f *testing.F) {
	bases := seedArtifacts(f)
	for i := range bases {
		v, err := NewView(bases[i])
		if err != nil {
			f.Fatal(err)
		}
		f.Add(uint8(i), uint32(offsetIn(bases[i], v.ID(0))), []byte{0x80}, false)
		f.Add(uint8(i), uint32(len(bases[i])/2), []byte{0xff, 0x00, 0x80}, true)
		f.Add(uint8(i), uint32(len(bases[i])-5), []byte{0x7f}, false)
		f.Add(uint8(i), uint32(rawFind(bases[i], KindRecords).off+9), []byte{0x20}, false)
	}
	f.Fuzz(func(t *testing.T, which uint8, pos uint32, x []byte, rh bool) {
		base := bases[int(which)%len(bases)]
		b := clone(base)
		for i, c := range x {
			b[(int(pos)+i)%len(b)] ^= c
		}
		if rh {
			rehash(b)
		}
		if v, ver, err := VerifyBytes(b, LevelTrusted); err == nil {
			if ver.ContentVerified || ver.BaseID != "" || ver.ContainerSHA256 != "" {
				t.Fatalf("trusted claims content: %+v", ver)
			}
			exercise(t, v)
		}
	})
}
