package artifact_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine/artifact"
	"github.com/kurn-dev/kurn/engine/ngram"
)

func buildIdx() *ngram.Index {
	b := ngram.NewBuilder(ngram.Config{Grams: []int{2, 3}, StripSpaces: true})
	b.Add(0, []string{"elena vasquez"})
	b.Add(1, []string{"marcus chen"})
	b.Add(2, []string{"dana kovak", "dana wilhelmina van der kovak"})
	return b.Finish()
}

// testDigest stands in for the analyzer-spec digest the store computes.
const testDigest = "0123456789ab"

func TestRoundTrip(t *testing.T) {
	idx := buildIdx()
	path := filepath.Join(t.TempDir(), "base.idx")
	if err := artifact.Save(path, idx, testDigest, artifact.BuildInfo{}); err != nil {
		t.Fatal(err)
	}
	loaded, digest, _, err := artifact.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if digest != testDigest {
		t.Fatalf("analyzer digest round-trip: got %q, want %q", digest, testDigest)
	}
	for _, q := range []string{"elena vasquez", "dana kovak", "elenavasquez", "zzz"} {
		want := idx.Lookup(q, 0.5, 10)
		got := loaded.Lookup(q, 0.5, 10)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Lookup(%q): loaded %+v != built %+v", q, got, want)
		}
	}
}

// TestDeterministic: identical indexes must serialize byte-identically
// (grams are written in sorted order, not random map order).
func TestDeterministic(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.idx")
	b := filepath.Join(dir, "b.idx")
	if err := artifact.Save(a, buildIdx(), testDigest, artifact.BuildInfo{}); err != nil {
		t.Fatal(err)
	}
	if err := artifact.Save(b, buildIdx(), testDigest, artifact.BuildInfo{}); err != nil {
		t.Fatal(err)
	}
	da, _ := os.ReadFile(a)
	db, _ := os.ReadFile(b)
	if !bytes.Equal(da, db) {
		t.Fatal("two saves of the same index differ byte-for-byte")
	}
}

// TestRestoreRejectsSmallNumOrds: a numOrds below the true max ordinal would
// make Lookup's dense accumulator panic, so Restore must reject it.
func TestRestoreRejectsSmallNumOrds(t *testing.T) {
	idx := buildIdx()
	if _, err := ngram.Restore(idx.Cfg(), idx.Postings(), idx.NumOrds()-1); err == nil {
		t.Fatal("want error for numOrds smaller than max ordinal + 1")
	}
	restored, err := ngram.Restore(idx.Cfg(), idx.Postings(), idx.NumOrds())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := restored.Lookup("dana kovak", 0.5, 10), idx.Lookup("dana kovak", 0.5, 10); !reflect.DeepEqual(got, want) {
		t.Errorf("restored Lookup %+v != original %+v", got, want)
	}
}

// TestRestoreRejectsBadGrams: gram size values are config from the artifact
// header — untrusted bytes. A negative size makes CharGrams panic (slice
// bounds); 0 silently posts every ordinal to the empty gram; absurdly large
// values are corrupt headers. Restore must reject them all.
func TestRestoreRejectsBadGrams(t *testing.T) {
	idx := buildIdx()
	for _, grams := range [][]int{{-1}, {0}, {2, 0}, {ngram.MaxGramSize + 1}, {}} {
		cfg := ngram.Config{Grams: grams, StripSpaces: true}
		if _, err := ngram.Restore(cfg, idx.Postings(), idx.NumOrds()); err == nil {
			t.Errorf("Restore accepted grams %v", grams)
		}
	}
	if _, err := ngram.Restore(idx.Cfg(), idx.Postings(), idx.NumOrds()); err != nil {
		t.Fatalf("Restore rejected the valid config: %v", err)
	}
}

// TestRejectHostileGrams: a crafted header with a hostile gram size must be
// rejected at Load — pre-fix, grams:[-1] loaded fine and the first Lookup
// panicked in CharGrams, and grams:[0] loaded and silently mis-indexed.
func TestRejectHostileGrams(t *testing.T) {
	for _, tc := range []string{
		`{"grams":[-1],"strip_spaces":true,"num_ords":0,"gram_count":0}`,
		`{"grams":[0],"strip_spaces":true,"num_ords":0,"gram_count":0}`,
		`{"grams":[2,99],"strip_spaces":true,"num_ords":0,"gram_count":0}`,
	} {
		hdr := []byte(tc)
		var buf bytes.Buffer
		buf.WriteString("KURNIDX1")
		var uv [binary.MaxVarintLen64]byte
		buf.Write(uv[:binary.PutUvarint(uv[:], uint64(len(hdr)))])
		buf.Write(hdr)
		path := filepath.Join(t.TempDir(), "hostile.idx")
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := artifact.Load(path); err == nil || !strings.Contains(err.Error(), "gram size") {
			t.Errorf("header %s: want gram-size rejection, got: %v", tc, err)
		}
	}
}

// TestRejectDuplicateGramKeys: a payload carrying the same gram twice must be
// rejected — map assignment would silently keep only the second bitmap
// (silently-wrong index), while exact.Restore rejects the analogous case.
// Save never produces duplicates (sorted unique map keys), so the file is
// crafted: a valid two-gram artifact with the second gram's bytes doctored to
// equal the first's.
func TestRejectDuplicateGramKeys(t *testing.T) {
	b := ngram.NewBuilder(ngram.Config{Grams: []int{2}, StripSpaces: true})
	b.Add(0, []string{"ab"})
	b.Add(1, []string{"cd"}) // exactly two grams: "ab", "cd"
	idx := b.Finish()
	path := filepath.Join(t.TempDir(), "dup.idx")
	if err := artifact.Save(path, idx, testDigest, artifact.BuildInfo{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Grams are written sorted, each as uvarint(len) + bytes: "ab" first,
	// "cd" second. Overwrite the single occurrence of "cd" with "ab".
	if n := bytes.Count(data, []byte("cd")); n != 1 {
		t.Fatalf("expected exactly one %q in the artifact, found %d", "cd", n)
	}
	doctored := bytes.Replace(data, []byte("cd"), []byte("ab"), 1)
	if err := os.WriteFile(path, doctored, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := artifact.Load(path); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate-gram rejection, got: %v", err)
	}
}

func TestRejectCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.idx")
	os.WriteFile(path, []byte("not an artifact"), 0o644)
	if _, _, _, err := artifact.Load(path); err == nil {
		t.Fatal("want error for corrupt file")
	}
	// truncated: write a valid one, cut it in half
	good := filepath.Join(t.TempDir(), "good.idx")
	if err := artifact.Save(good, buildIdx(), testDigest, artifact.BuildInfo{}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(good)
	os.WriteFile(path, data[:len(data)/2], 0o644)
	if _, _, _, err := artifact.Load(path); err == nil {
		t.Fatal("want error for truncated file")
	}
	// trailing data: a valid artifact with extra bytes appended
	os.WriteFile(path, append(append([]byte{}, data...), 0xff), 0o644)
	if _, _, _, err := artifact.Load(path); err == nil {
		t.Fatal("want error for trailing data")
	}
}

// TestRejectHostileGramCount: a crafted header claiming an absurd gram_count
// must error immediately (plausibility bound against the file size) instead of
// preallocating a gigabyte-scale postings map.
func TestRejectHostileGramCount(t *testing.T) {
	hdr := []byte(`{"grams":[2,3],"strip_spaces":true,"num_ords":3,"gram_count":50000000}`)
	var buf bytes.Buffer
	buf.WriteString("KURNIDX1")
	var uv [binary.MaxVarintLen64]byte
	buf.Write(uv[:binary.PutUvarint(uv[:], uint64(len(hdr)))])
	buf.Write(hdr)
	path := filepath.Join(t.TempDir(), "hostile.idx")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	// Require the plausibility rejection specifically: pre-fix code also
	// errored eventually, but only after preallocating a ~1.8GB postings map.
	if _, _, _, err := artifact.Load(path); err == nil || !strings.Contains(err.Error(), "implausible") {
		t.Fatalf("want plausibility-bound error for hostile gram_count, got: %v", err)
	}
}

// buildBigIdx builds a deterministic ~100k-distinct-gram index (seeded
// generator) for the Save/Load benchmarks.
func buildBigIdx(tb testing.TB) *ngram.Index {
	rng := rand.New(rand.NewSource(7))
	const letters = "abcdefghijklmnopqrstuvwxyz"
	b := ngram.NewBuilder(ngram.Config{Grams: []int{2, 3, 4}, StripSpaces: true})
	key := make([]byte, 24)
	for ord := uint32(0); ord < 10000; ord++ {
		for i := range key {
			key[i] = letters[rng.Intn(len(letters))]
		}
		b.Add(ord, []string{string(key)})
	}
	idx := b.Finish()
	ords, grams := idx.Stats()
	tb.Logf("bench index: %d ords, %d distinct grams", ords, grams)
	return idx
}

func BenchmarkSave(b *testing.B) {
	idx := buildBigIdx(b)
	dir := b.TempDir()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := artifact.Save(filepath.Join(dir, fmt.Sprintf("big-%d.idx", i)), idx, testDigest, artifact.BuildInfo{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLoad(b *testing.B) {
	idx := buildBigIdx(b)
	path := filepath.Join(b.TempDir(), "big.idx")
	if err := artifact.Save(path, idx, testDigest, artifact.BuildInfo{}); err != nil {
		b.Fatal(err)
	}
	if fi, err := os.Stat(path); err == nil {
		b.Logf("artifact size: %d bytes", fi.Size())
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, err := artifact.Load(path); err != nil {
			b.Fatal(err)
		}
	}
}

// A persisted build record with impossible values is corrupt metadata and
// must fail the load, so the store rebuilds instead of serving it.
func TestLoadRejectsImpossibleBuildRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.idx")
	// Two families: values the relational NumOrds guard also catches, and
	// ones ONLY the record's own validation can (negative loss counts and
	// keyless > entries are invisible to the ordinal arithmetic).
	for name, b := range map[string]artifact.BuildInfo{
		"negative entries":     {BaseID: "abc", Entries: -1},
		"negative counts":      {BaseID: "abc", Entries: 3, DroppedKeys: -2, KeylessEntries: -3},
		"keyless past entries": {BaseID: "abc", Entries: 3, KeylessEntries: 4},
	} {
		if err := artifact.Save(path, buildIdx(), testDigest, b); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := artifact.Load(path); err == nil {
			t.Errorf("%s: record %+v loaded cleanly", name, b)
		}
	}
}
