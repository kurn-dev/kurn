package prepared

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests of LevelTrusted in VerifyBytes: which checks it retains and which
// it skips, the payload rule, and the unchanged meaning of the older
// levels.

// setSectionDigest writes section kind's actual SHA-256 into its table
// entry and leaves the header digest as it is.
func setSectionDigest(buf []byte, kind uint32) {
	s := rawFind(buf, kind)
	sum := sha256.Sum256(buf[s.off:s.end()])
	copy(buf[headerSize+s.idx*tableEntrySize+24:], sum[:])
}

// rehashHeaderOnly recomputes only the header and table digest.
func rehashHeaderOnly(buf []byte) {
	hl := headerSize + len(rawTable(buf))*tableEntrySize
	sum := sha256.Sum256(buf[:hl])
	copy(buf[hl:], sum[:])
}

// patchRecords replaces the first occurrence of old in the records section
// with new (same length), touching no digest.
func patchRecords(t *testing.T, buf []byte, old, new string) []byte {
	t.Helper()
	if len(old) != len(new) {
		t.Fatalf("patch changes length")
	}
	b := clone(buf)
	s := rawFind(b, KindRecords)
	body := b[s.off:s.end()]
	i := bytes.Index(body, []byte(old))
	if i < 0 {
		t.Fatalf("%q not in the records section", old)
	}
	copy(body[i:], new)
	return b
}

func TestLevelValuesUnchanged(t *testing.T) {
	if LevelFull != 0 || LevelStructure != 1 || LevelTrusted != 2 {
		t.Fatalf("levels %d %d %d", LevelFull, LevelStructure, LevelTrusted)
	}
	var zero Level
	if zero != LevelFull || zero.String() != "full" || LevelStructure.String() != "structure" || LevelTrusted.String() != "trusted" || Level(3).String() != "Level(3)" {
		t.Fatal("level names or default changed")
	}
	if _, _, err := VerifyBytes(artifactT(t, "exact", 3), Level(3)); err == nil {
		t.Fatal("unknown level accepted")
	}
}

// Trusted establishes no verified identity, reports what it checked, and
// yields the same View contents as full.
func TestTrustedVerifiedFields(t *testing.T) {
	for _, mode := range []string{"ngram", "exact"} {
		buf := artifactT(t, mode, 200)
		vf, full, err := VerifyBytes(buf, LevelFull)
		if err != nil {
			t.Fatal(err)
		}
		vt, tr, err := VerifyBytes(buf, LevelTrusted)
		if err != nil {
			t.Fatal(err)
		}
		_, st, err := VerifyBytes(buf, LevelStructure)
		if err != nil {
			t.Fatal(err)
		}
		if !full.HeaderDigestVerified || !full.ContentVerified || full.BaseID == "" || full.ContainerSHA256 == "" || full.Level != LevelFull {
			t.Fatalf("%s full: %+v", mode, full)
		}
		if tr.Level != LevelTrusted || !tr.HeaderDigestVerified || tr.ContentVerified || tr.BaseID != "" || tr.ContainerSHA256 != "" || tr.DeclaredBaseID != full.BaseID {
			t.Fatalf("%s trusted: %+v", mode, tr)
		}
		if st.Level != LevelStructure || st.HeaderDigestVerified || st.ContentVerified || st.BaseID != "" || st.ContainerSHA256 != "" || st.DeclaredBaseID != full.BaseID {
			t.Fatalf("%s structure: %+v", mode, st)
		}
		// Everything but the level-dependent fields agrees.
		a, b := *full, *tr
		a.Level, a.BaseID, a.ContainerSHA256, a.ContentVerified = b.Level, "", "", false
		if a != b {
			t.Fatalf("%s: trusted %+v, full %+v", mode, b, *full)
		}
		for i := 0; i < vf.Len(); i++ {
			if vf.ID(i) != vt.ID(i) || !bytes.Equal(vf.Payload(i), vt.Payload(i)) || strings.Join(vf.Keys(i), "\x00") != strings.Join(vt.Keys(i), "\x00") {
				t.Fatalf("%s: record %d differs", mode, i)
			}
		}
		if mode == "ngram" && vt.GramCount() != vf.GramCount() {
			t.Fatal("gram counts differ")
		}
		if v, err := Verify(writeTempArtifact(t, buf), LevelTrusted); err != nil || *v != *tr {
			t.Fatalf("Verify(path, trusted): %+v %v", v, err)
		}
	}
}

func writeTempArtifact(t *testing.T, buf []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.kart")
	if err := os.WriteFile(p, buf, 0o444); err != nil {
		t.Fatal(err)
	}
	return p
}

// The three content sections' digests are skipped under trusted; every
// other digest is retained. Each case writes a wrong digest into one
// table entry and recomputes the header digest, so only that section's
// digest check can refuse.
func TestTrustedRetainedAndSkippedDigests(t *testing.T) {
	for _, mode := range []string{"ngram", "exact"} {
		buf := withExtraSection(t, artifactT(t, mode, 60), 200, 0, []byte("optional future data"))
		rehash(buf)
		index := KindNgram
		if mode == "exact" {
			index = KindExact
		}
		for _, tc := range []struct {
			kind    uint32
			skipped bool
		}{
			{KindConfig, false}, {KindBuild, false}, {200, false},
			{KindRecords, true}, {KindIDs, true}, {index, true},
		} {
			b := clone(buf)
			s := rawFind(b, tc.kind)
			b[headerSize+s.idx*tableEntrySize+24] ^= 1
			rehashHeaderOnly(b)
			if _, _, err := VerifyBytes(b, LevelFull); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("%s kind %d: full accepted a wrong section digest: %v", mode, tc.kind, err)
			}
			_, ver, err := VerifyBytes(b, LevelTrusted)
			if tc.skipped {
				if err != nil || ver.ContentVerified {
					t.Fatalf("%s kind %d: trusted checked a skipped content digest: %v", mode, tc.kind, err)
				}
			} else if !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "section kind") {
				t.Fatalf("%s kind %d: trusted skipped a retained digest: %v", mode, tc.kind, err)
			}
		}
		// The header and table digest itself is retained.
		b := clone(buf)
		b[headerSize+rawFind(b, KindRecords).idx*tableEntrySize+24] ^= 1
		for _, lvl := range []Level{LevelFull, LevelTrusted} {
			if _, _, err := VerifyBytes(b, lvl); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "header") {
				t.Fatalf("%s %v: stale header digest: %v", mode, lvl, err)
			}
		}
		if _, _, err := VerifyBytes(b, LevelStructure); err != nil {
			t.Fatalf("structure level changed meaning: %v", err)
		}
	}
}

// The observable assurance difference: a records byte altered without
// updating its recorded digest opens under trusted, serving the altered
// bytes, and is refused under full. Written into the table with the
// header digest left stale, both refuse.
func TestTrustedAssuranceDifference(t *testing.T) {
	buf := artifactT(t, "ngram", 80)
	b := patchRecords(t, buf, `"SDGT"`, `"SDGX"`)
	if _, _, err := VerifyBytes(b, LevelFull); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "section kind 3") {
		t.Fatalf("full: %v", err)
	}
	v, ver, err := VerifyBytes(b, LevelTrusted)
	if err != nil {
		t.Fatalf("trusted refused a structurally valid content change: %v", err)
	}
	found := false
	for i := 0; i < v.Len(); i++ {
		found = found || bytes.Contains(v.Payload(i), []byte(`"SDGX"`))
	}
	if !found || ver.ContentVerified || ver.BaseID != "" {
		t.Fatalf("trusted view does not serve the altered bytes, or claims content: %+v", ver)
	}
	setSectionDigest(b, KindRecords) // the table now records the altered digest; header stale
	for _, lvl := range []Level{LevelFull, LevelTrusted} {
		if _, _, err := VerifyBytes(b, lvl); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "header") {
			t.Fatalf("%v: %v", lvl, err)
		}
	}
}

// An unknown optional section is hashed under trusted: its bytes altered
// without updating its digest are refused.
func TestTrustedHashesUnknownOptionalSection(t *testing.T) {
	buf := withExtraSection(t, artifactT(t, "exact", 30), 200, 0, []byte("future data"))
	rehash(buf)
	if _, _, err := VerifyBytes(buf, LevelTrusted); err != nil {
		t.Fatal(err)
	}
	b := clone(buf)
	s := rawFind(b, 200)
	b[s.off] ^= 1
	for _, lvl := range []Level{LevelFull, LevelTrusted} {
		if _, _, err := VerifyBytes(b, lvl); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "section kind 200") {
			t.Fatalf("%v: %v", lvl, err)
		}
	}
}

// The payload rule: a non-empty payload must be valid JSON under trusted
// (ErrMalformed); valid but non-canonical is accepted under trusted and
// refused under full; nil or empty payloads are accepted by both.
func TestTrustedPayloadRule(t *testing.T) {
	buf := artifactT(t, "ngram", 80)

	invalid := patchRecords(t, buf, `["SDGT"`, `{"SDGT"`)
	rehash(invalid)
	if _, _, err := VerifyBytes(invalid, LevelTrusted); !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("trusted, invalid JSON: %v", err)
	}
	if _, _, err := VerifyBytes(invalid, LevelFull); err == nil {
		t.Fatal("full accepted invalid JSON")
	}

	noncanon := patchRecords(t, buf, `["SDGT"`, `[ "SDG"`)
	rehash(noncanon)
	if _, _, err := VerifyBytes(noncanon, LevelTrusted); err != nil {
		t.Fatalf("trusted refused valid non-canonical JSON: %v", err)
	}
	if _, _, err := VerifyBytes(noncanon, LevelFull); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("full, non-canonical: %v", err)
	}
	// json.Valid is the rule, literally: it accepts invalid UTF-8 inside a
	// string, so trusted opens such a payload (a content change that stays
	// syntactically valid); full refuses it through its content checks.
	badUTF8 := patchRecords(t, buf, `"SDGT"`, "\"SDG\xff\"")
	rehash(badUTF8)
	if _, _, err := VerifyBytes(badUTF8, LevelTrusted); err != nil {
		t.Fatalf("trusted, invalid UTF-8 inside a JSON string: %v", err)
	}
	if _, _, err := VerifyBytes(badUTF8, LevelFull); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("full, invalid UTF-8 inside a JSON string: %v", err)
	}
	// Structure is unchanged: it checks no payload syntax.
	if _, _, err := VerifyBytes(invalid, LevelStructure); err != nil {
		t.Fatalf("structure level changed meaning: %v", err)
	}

	// Empty payloads (testRecords leaves every fifth record without one).
	vf, _, err := VerifyBytes(buf, LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	vt, _, err := VerifyBytes(buf, LevelTrusted)
	if err != nil {
		t.Fatal(err)
	}
	empty := 0
	for i := 0; i < vt.Len(); i++ {
		if vt.Payload(i) == nil {
			empty++
		}
		if !bytes.Equal(vt.Payload(i), vf.Payload(i)) {
			t.Fatalf("record %d differs", i)
		}
	}
	if empty == 0 {
		t.Fatal("fixture has no empty payloads")
	}
}

// A trusted artifact whose every payload is empty.
func TestTrustedAllEmptyPayloads(t *testing.T) {
	recs := testRecords(40, 3)
	for i := range recs {
		recs[i].Payload = nil
	}
	buf, _ := encodeT(t, testContents("exact", recs))
	for _, lvl := range []Level{LevelFull, LevelTrusted} {
		if _, _, err := VerifyBytes(buf, lvl); err != nil {
			t.Fatalf("%v: %v", lvl, err)
		}
	}
}
