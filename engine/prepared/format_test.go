package prepared

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestRoundTripNgram(t *testing.T) {
	recs := testRecords(300, 7)
	c := testContents("ngram", recs)
	buf, sum := encodeT(t, c)
	v, ver, err := VerifyBytes(buf, LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	if ver.BaseID != sum.BaseID || ver.ContainerSHA256 != sum.ContainerSHA256 {
		t.Fatalf("verified identity %+v disagrees with encoder %+v", ver, sum)
	}
	checkRecords(t, v, recs)
	if v.NumOrds() != c.Ngram.NumOrds() {
		t.Fatalf("numOrds %d, want %d", v.NumOrds(), c.Ngram.NumOrds())
	}
	post := c.Ngram.Postings()
	if v.GramCount() != len(post) {
		t.Fatalf("%d grams, want %d", v.GramCount(), len(post))
	}
	for i := 0; i < v.GramCount(); i++ {
		g := v.Gram(i)
		bm, err := v.Postings(i)
		if err != nil {
			t.Fatal(err)
		}
		want := post[g]
		if want == nil || !bm.Equals(want) || bm.GetCardinality() != want.GetCardinality() {
			t.Fatalf("gram %q: bitmap %v, want %v", g, bm.ToArray(), want)
		}
	}
	idx, err := v.NgramIndex()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"anna smith", "acme holdings", "müller", "zeta", "nothing here"} {
		got, want := idx.Lookup(q, 0.3, 50), c.Ngram.Lookup(q, 0.3, 50)
		if !slices.Equal(got, want) {
			t.Fatalf("lookup %q: %v, want %v", q, got, want)
		}
	}
}

func TestRoundTripExact(t *testing.T) {
	recs := testRecords(300, 9)
	c := testContents("exact", recs)
	buf, _ := encodeT(t, c)
	v, _, err := VerifyBytes(buf, LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	checkRecords(t, v, recs)
	idx, err := v.ExactIndex()
	if err != nil {
		t.Fatal(err)
	}
	if idx.Keys() != c.Exact.Keys() || idx.NumOrds() != c.Exact.NumOrds() || v.NumOrds() != c.Exact.NumOrds() {
		t.Fatalf("restored %d keys/%d ords, want %d/%d", idx.Keys(), idx.NumOrds(), c.Exact.Keys(), c.Exact.NumOrds())
	}
	for _, k := range c.Exact.SortedKeys() {
		if !slices.Equal(idx.Lookup(k), c.Exact.Lookup(k)) {
			t.Fatalf("key %q: %v, want %v", k, idx.Lookup(k), c.Exact.Lookup(k))
		}
	}
	if idx.Lookup("absent key") != nil {
		t.Fatal("absent key found")
	}
}

// checkRecords requires every record's ID, keys (nil-ness included) and
// payload to equal the input, and the ids section to be a sorted index.
func checkRecords(t *testing.T, v *View, recs []Record) {
	t.Helper()
	if v.Len() != len(recs) {
		t.Fatalf("%d records, want %d", v.Len(), len(recs))
	}
	for i, r := range recs {
		if v.ID(i) != r.ID {
			t.Fatalf("record %d: id %q, want %q", i, v.ID(i), r.ID)
		}
		keys := v.Keys(i)
		if (keys == nil) != (r.Keys == nil) || !slices.Equal(keys, r.Keys) {
			t.Fatalf("record %d: keys %#v, want %#v", i, keys, r.Keys)
		}
		p := v.Payload(i)
		if !bytes.Equal(p, r.Payload) || cap(p) != len(p) {
			t.Fatalf("record %d: payload %q (cap %d), want %q", i, p, cap(p), r.Payload)
		}
		if o, ok := v.Lookup(r.ID); !ok || o != i {
			t.Fatalf("Lookup(%q) = %d, %v; want %d", r.ID, o, ok, i)
		}
	}
	if _, ok := v.Lookup("no-such-id"); ok {
		t.Fatal("found an absent ID")
	}
	ids := make([]string, len(recs))
	for k := range ids {
		ids[k] = v.ID(v.IDOrdinal(k))
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("ids section not sorted")
	}
}

func TestBaseIDIsCanonicalLineHash(t *testing.T) {
	// The empty-but-not-null key list comes FIRST: verification reuses a
	// key buffer across records, and it must not start out nil.
	recs := []Record{
		{ID: "e", Keys: []string{}, Payload: mustCanon("null")},
		{ID: "a<b>&c", Keys: []string{"\u2028x\u2029", `q"uote`, "ünï"}, Payload: mustCanon(`{"x" : "<&>" }`)},
		{ID: "n", Keys: nil},
		{ID: "z", Keys: []string{}},
	}
	var want bytes.Buffer
	for _, r := range recs {
		line, err := json.Marshal(&canonEntry{ID: r.ID, Keys: r.Keys, Payload: r.Payload})
		if err != nil {
			t.Fatal(err)
		}
		want.Write(line)
		want.WriteByte('\n')
	}
	if !strings.Contains(want.String(), `"keys":null`) || !strings.Contains(want.String(), `"keys":[]`) {
		t.Fatalf("fixture lost its null/empty key distinction: %s", want.String())
	}
	sum := sha256.Sum256(want.Bytes())
	buf, s := encodeT(t, testContents("exact", recs))
	if s.BaseID != hex.EncodeToString(sum[:]) {
		t.Fatalf("base ID %s, want %x", s.BaseID, sum)
	}
	// And verification's re-encoding reproduces it from the records.
	v, ver, err := VerifyBytes(buf, LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	if ver.BaseID != s.BaseID {
		t.Fatalf("verified base ID %s, encoded %s", ver.BaseID, s.BaseID)
	}
	checkRecords(t, v, recs)
}

func TestEncodeDeterministic(t *testing.T) {
	for _, mode := range []string{"ngram", "exact"} {
		a, _ := encodeT(t, testContents(mode, testRecords(200, 3)))
		b, _ := encodeT(t, testContents(mode, testRecords(200, 3)))
		if !bytes.Equal(a, b) {
			t.Fatalf("%s: two encodings of the same contents differ", mode)
		}
	}
}

func TestEncodeRefuses(t *testing.T) {
	for name, mut := range map[string]func(c *Contents){
		"bad name":          func(c *Contents) { c.Name = "Bad Name" },
		"empty id":          func(c *Contents) { c.Records[1].ID = "" },
		"duplicate id":      func(c *Contents) { c.Records[2].ID = c.Records[1].ID },
		"non-canonical":     func(c *Contents) { c.Records[0].Payload = []byte(`{"a": 1}`) },
		"invalid payload":   func(c *Contents) { c.Records[0].Payload = []byte(`{"a":`) },
		"oversize line":     func(c *Contents) { c.Records[0].Payload = mustCanon(`"` + strings.Repeat("x", MaxLine) + `"`) },
		"wrong index":       func(c *Contents) { c.Ngram, c.Exact = nil, buildExact(c.Records) },
		"bad digest":        func(c *Contents) { c.ConfigDigest = "XYZ" },
		"bad mode":          func(c *Contents) { c.ListConfig = json.RawMessage(`{"match":{"mode":"fuzzy"}}`) },
		"grams disagree":    func(c *Contents) { c.Ngram = buildNgram(c.Records, []int{3}, false) },
		"impossible counts": func(c *Contents) { c.KeylessEntries = len(c.Records) + 1 },
	} {
		c := testContents("ngram", testRecords(20, 2))
		mut(c)
		if _, _, err := Encode(c); err == nil {
			t.Errorf("%s: Encode accepted it", name)
		}
	}
}

func TestInspectReturnsDeclaredWithoutVerifying(t *testing.T) {
	c := testContents("ngram", testRecords(50, 4))
	c.Source = "feed@2026-09-29"
	buf, sum := encodeT(t, c)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.kurn")
	// Corrupt a records byte WITHOUT rehashing: Inspect must not notice
	// (it verifies nothing), Verify must.
	rec := rawFind(buf, KindRecords)
	bad := clone(buf)
	bad[rec.off+rec.length-1] ^= 0xff
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Info{Format: "KURNART1 1.1", Major: 1, Minor: 1, Name: "test-list", Mode: "ngram", Entries: 50,
		BaseID: sum.BaseID, ConfigDigest: fakeConfigDigest, AnalyzerDigest: fakeAnalyzerDigest, Size: int64(len(buf))}
	if *info != want {
		t.Fatalf("Inspect = %+v, want %+v", *info, want)
	}
	if _, err := Verify(path, LevelFull); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("Verify of a corrupted file: %v, want ErrIntegrity", err)
	}
	// A declared base ID the records do not reproduce also passes Inspect.
	lie := clone(buf)
	b := rawFind(lie, KindBuild)
	body := lie[b.off : b.off+b.length]
	i := bytes.Index(body, []byte(sum.BaseID))
	copy(body[i:], strings.Repeat("0", 64))
	rehash(lie)
	path2 := filepath.Join(dir, "lie.kurn")
	if err := os.WriteFile(path2, lie, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err = Inspect(path2)
	if err != nil || info.BaseID != strings.Repeat("0", 64) {
		t.Fatalf("Inspect = %+v, %v; want the declared (false) base ID", info, err)
	}
	if _, err := Verify(path2, LevelFull); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "records hash to") {
		t.Fatalf("Verify of a false base ID: %v", err)
	}
}

func TestVerifyCatchesTampering(t *testing.T) {
	buf := artifactT(t, "ngram", 80)
	rec := rawFind(buf, KindRecords)

	t.Run("section digest", func(t *testing.T) {
		b := clone(buf)
		e := b[headerSize+rec.idx*tableEntrySize:]
		e[30] ^= 1 // inside the records section's stored digest
		// Header digest recomputed, so only the section digest disagrees.
		hl := headerSize + len(rawTable(b))*tableEntrySize
		s := sha256.Sum256(b[:hl])
		copy(b[hl:], s[:])
		if _, _, err := VerifyBytes(b, LevelFull); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "section kind 3") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("header digest", func(t *testing.T) {
		b := clone(buf)
		e := b[headerSize+rec.idx*tableEntrySize:]
		e[30] ^= 1
		if _, _, err := VerifyBytes(b, LevelFull); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "header") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("data byte", func(t *testing.T) {
		b := clone(buf)
		b[rec.off+rec.length-2] ^= 0x20 // inside a payload/blob byte
		if _, _, err := VerifyBytes(b, LevelFull); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("got %v", err)
		}
		// The structure level does not hash, and the flip is structurally
		// harmless — which is exactly why full verification is the default.
		if _, _, err := VerifyBytes(b, LevelStructure); err != nil {
			t.Fatalf("structure level: %v", err)
		}
	})
	t.Run("rehashed payload bytes", func(t *testing.T) {
		// Same-length change to a payload, digests recomputed: only the
		// records re-hash (base ID recomputation) can catch it.
		b := clone(buf)
		body := b[rec.off : rec.off+rec.length]
		i := bytes.Index(body, []byte(`"SDGT"`))
		copy(body[i:], `"SDGX"`)
		rehash(b)
		if _, _, err := VerifyBytes(b, LevelFull); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "records hash to") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("non-canonical payload", func(t *testing.T) {
		b := clone(buf)
		body := b[rec.off : rec.off+rec.length]
		i := bytes.Index(body, []byte(`["SDGT"`))
		copy(body[i:], `[ "SDG"`) // same length, now with a space
		rehash(b)
		if _, _, err := VerifyBytes(b, LevelFull); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "canonical") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestStructureLevelEstablishesNoIdentity(t *testing.T) {
	buf := artifactT(t, "exact", 10)
	_, ver, err := VerifyBytes(buf, LevelStructure)
	if err != nil {
		t.Fatal(err)
	}
	if ver.BaseID != "" || ver.ContainerSHA256 != "" || ver.DeclaredBaseID == "" {
		t.Fatalf("structure level reported identity: %+v", ver)
	}
	if _, _, err := VerifyBytes(buf, Level(7)); err == nil {
		t.Fatal("unknown level accepted")
	}
}

func TestViewRequiresAlignedBuffer(t *testing.T) {
	buf := artifactT(t, "ngram", 10)
	big := AlignedBuffer(len(buf) + 1)
	copy(big[1:], buf)
	if _, err := NewView(big[1:]); !errors.Is(err, ErrUnaligned) {
		t.Fatalf("got %v, want ErrUnaligned", err)
	}
	if !strings.Contains(ErrUnaligned.Error(), "32-byte aligned") {
		t.Fatal("alignment error does not say what is required")
	}
}

func TestVerifyFileAndReadFile(t *testing.T) {
	buf, sum := encodeT(t, testContents("exact", testRecords(40, 5)))
	path := filepath.Join(t.TempDir(), "x.kurn")
	if err := os.WriteFile(path, buf, 0o444); err != nil {
		t.Fatal(err)
	}
	ver, err := Verify(path, LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	fileSum := sha256.Sum256(buf)
	if ver.ContainerSHA256 != hex.EncodeToString(fileSum[:]) || ver.BaseID != sum.BaseID || ver.Name != "test-list" || ver.Mode != "exact" || ver.Entries != 40 {
		t.Fatalf("Verify = %+v", ver)
	}
	if _, err := Verify(filepath.Dir(path), LevelFull); err == nil {
		t.Fatal("verified a directory")
	}
}

func TestUnknownOptionalSectionAndMinorVersionLoad(t *testing.T) {
	buf := artifactT(t, "ngram", 30)
	// Append an unknown OPTIONAL section and bump the minor version: a
	// reader of 1.0 must load it.
	b := withExtraSection(t, buf, 200, 0, []byte("future data"))
	b[10], b[11] = 5, 0 // minor version 5 (u16 at offset 10)
	rehash(b)
	if _, _, err := VerifyBytes(b, LevelFull); err != nil {
		t.Fatalf("unknown optional section / higher minor: %v", err)
	}
	// The same section flagged required is an incompatibility.
	b = withExtraSection(t, buf, 200, flagRequired, []byte("future data"))
	rehash(b)
	if _, _, err := VerifyBytes(b, LevelFull); !errors.Is(err, ErrIncompatible) || !strings.Contains(err.Error(), "kind 200") {
		t.Fatalf("unknown required section: %v", err)
	}
}

// withExtraSection re-lays buf with one more section appended at the end.
func withExtraSection(t *testing.T, buf []byte, kind, flags uint32, data []byte) []byte {
	t.Helper()
	old := rawTable(buf)
	secs := make([]section, 0, len(old)+1)
	bodies := [][]byte{}
	for _, s := range old {
		secs = append(secs, section{kind: s.kind, flags: s.flags, length: s.length})
		bodies = append(bodies, buf[s.off:s.off+s.length])
	}
	secs = append(secs, section{kind: kind, flags: flags, length: uint64(len(data))})
	bodies = append(bodies, data)
	off, _ := align32(uint64(headerSize + len(secs)*tableEntrySize + digestSize))
	for i := range secs {
		secs[i].off = off
		off, _ = align32(secs[i].end())
	}
	out := AlignedBuffer(int(secs[len(secs)-1].end()))
	copy(out, encodeHead(secs))
	for i, s := range secs {
		copy(out[s.off:], bodies[i])
	}
	return out
}
