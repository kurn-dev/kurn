package prepared

import (
	"errors"
	"strings"
	"testing"
	"unsafe"
)

// TestEncodeRefusesInvalidUTF8 pins F1: json.Marshal coerces each invalid
// byte to U+FFFD, so "a\xff" and "a�" would hash to the same base ID
// while serving different bytes. Encode refuses the invalid one outright.
func TestEncodeRefusesInvalidUTF8(t *testing.T) {
	for name, rec := range map[string]Record{
		"id and key": {ID: "a\xff", Keys: []string{"k\xff"}},
		"id":         {ID: "a\xff", Keys: []string{"k"}},
		"key":        {ID: "a", Keys: []string{"ok", "k\xff"}},
		"truncated":  {ID: "a\xe2\x82", Keys: []string{"k"}},
	} {
		recs := append(testRecords(5, 1), rec)
		_, _, err := Encode(testContents("exact", recs))
		if err == nil || !strings.Contains(err.Error(), "not valid UTF-8") || !strings.Contains(err.Error(), "record 5") {
			t.Errorf("%s: Encode = %v, want a UTF-8 refusal naming record 5", name, err)
		}
	}
	// The valid spelling of the collision partner is accepted, and nothing
	// else can reach its base ID any more.
	if _, _, err := Encode(testContents("exact", []Record{{ID: "a�", Keys: []string{"k�"}}})); err != nil {
		t.Fatalf("valid U+FFFD record refused: %v", err)
	}
}

// offsetIn returns the offset of s's bytes inside buf.
func offsetIn(buf []byte, s string) int {
	return int(uintptr(unsafe.Pointer(unsafe.StringData(s))) - uintptr(unsafe.Pointer(&buf[0])))
}

// invalidUTF8Artifact patches the last byte of record 3's ID (or its key) to
// 0xff, sets the declared base ID to exactly what the line hasher computes
// for the patched records (json.Marshal coerces the byte, so that hash
// exists and is computable), and recomputes every digest. Everything except
// the UTF-8 structural rule therefore passes.
func invalidUTF8Artifact(t testing.TB, mode string, inKey bool) []byte {
	recs := testRecords(12, 1)
	recs[3].Keys = []string{"zeta holdings"}
	buf, _ := encodeT(t, testContents(mode, recs))
	v, err := NewView(buf)
	if err != nil {
		t.Fatal(err)
	}
	target := v.ID(3)
	if inKey {
		target = v.Key(3, 0)
	}
	b := clone(buf)
	at := offsetIn(buf, target)
	// The last byte: this keeps the ids section's bytewise order intact
	// (no ID lies between "id-23757" and "id-2375\xff" in the fixture).
	b[at+len(target)-1] = 0xff
	patched := string(b[at : at+len(target)])
	lh := newLineHasher()
	for i, r := range recs {
		id, keys := r.ID, r.Keys
		if i == 3 && inKey {
			keys = []string{patched}
		} else if i == 3 {
			id = patched
		}
		if _, err := lh.add(id, keys, r.Payload); err != nil {
			t.Fatal(err)
		}
	}
	base := lh.sum()
	b = editBuild(b, func(bi *BuildInfo) { bi.BaseID = base })
	rehash(b)
	return b
}

func TestInvalidUTF8IsMalformedAtEveryLevel(t *testing.T) {
	for _, mode := range []string{"ngram", "exact"} {
		for _, inKey := range []bool{false, true} {
			b := invalidUTF8Artifact(t, mode, inKey)
			what := map[bool]string{false: "id", true: "key"}[inKey]
			for _, level := range []Level{LevelStructure, LevelFull} {
				_, _, err := VerifyBytes(b, level)
				if !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), "record 3: "+what+" is not valid UTF-8") {
					t.Errorf("%s/%s/%v: got %v", mode, what, level, err)
				}
			}
			if _, err := NewView(b); !errors.Is(err, ErrMalformed) {
				t.Errorf("%s/%s: NewView accepted it", mode, what)
			}
		}
	}
}
