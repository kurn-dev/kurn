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
	"github.com/kurn-dev/kurn/engine/exact"
)

// buildExactIdx builds a small index covering singles, shared keys (multi-ord
// runs), and multi-key entries.
func buildExactIdx(tb testing.TB) *exact.Index {
	tb.Helper()
	b := exact.NewBuilder()
	b.Add(0, []string{"bad@example.com"})
	b.Add(1, []string{"worse@example.com", "alias@example.com"})
	b.Add(2, []string{"bad@example.com", "third@example.com"}) // shares a key with ord 0
	idx, err := b.Finish()
	if err != nil {
		tb.Fatalf("Finish: %v", err)
	}
	return idx
}

// TestExactRoundTrip: a loaded artifact must answer every Lookup exactly like
// the index it was saved from (hits, multi-ord runs, and misses).
func TestExactRoundTrip(t *testing.T) {
	idx := buildExactIdx(t)
	path := filepath.Join(t.TempDir(), "base.idx")
	if err := artifact.SaveExact(path, idx, testDigest); err != nil {
		t.Fatal(err)
	}
	loaded, digest, err := artifact.LoadExact(path)
	if err != nil {
		t.Fatal(err)
	}
	if digest != testDigest {
		t.Fatalf("analyzer digest round-trip: got %q, want %q", digest, testDigest)
	}
	for _, q := range []string{"bad@example.com", "worse@example.com", "alias@example.com", "third@example.com", "", "miss@example.com"} {
		want := idx.Lookup(q)
		got := loaded.Lookup(q)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Lookup(%q): loaded %v != built %v", q, got, want)
		}
	}
	if got, want := loaded.NumOrds(), idx.NumOrds(); got != want {
		t.Errorf("NumOrds: loaded %d != built %d", got, want)
	}
	if got, want := loaded.Keys(), idx.Keys(); got != want {
		t.Errorf("Keys: loaded %d != built %d", got, want)
	}
}

// TestExactRoundTripDifferential: randomized shapes (singles, hot shared
// keys, duplicate keys within one Add) survive a save/load cycle verbatim.
func TestExactRoundTripDifferential(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	ref := map[string][]uint32{}
	b := exact.NewBuilder()
	for ord := uint32(0); ord < 2000; ord++ {
		nKeys := 1 + r.Intn(3)
		keys := make([]string, 0, nKeys)
		for k := 0; k < nKeys; k++ {
			var key string
			if r.Intn(10) == 0 && ord > 0 {
				key = fmt.Sprintf("shared-%d", r.Intn(50))
			} else {
				key = fmt.Sprintf("key-%d-%d", ord, k)
			}
			keys = append(keys, key)
			if p := ref[key]; len(p) == 0 || p[len(p)-1] != ord {
				ref[key] = append(ref[key], ord)
			}
		}
		b.Add(ord, keys)
	}
	idx, err := b.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	path := filepath.Join(t.TempDir(), "base.idx")
	if err := artifact.SaveExact(path, idx, testDigest); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := artifact.LoadExact(path)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range ref {
		if got := loaded.Lookup(key); !reflect.DeepEqual(got, want) {
			t.Errorf("Lookup(%q) = %v, want %v", key, got, want)
		}
	}
	for _, miss := range []string{"", "nope", "key-99999-0"} {
		if got := loaded.Lookup(miss); got != nil {
			t.Errorf("Lookup(%q) = %v, want nil", miss, got)
		}
	}
}

// TestExactDeterministic: identical indexes must serialize byte-identically
// even though Finish's map-iteration layout is nondeterministic (keys are
// re-canonicalized in sorted order on save).
func TestExactDeterministic(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.idx")
	b := filepath.Join(dir, "b.idx")
	if err := artifact.SaveExact(a, buildExactIdx(t), testDigest); err != nil {
		t.Fatal(err)
	}
	if err := artifact.SaveExact(b, buildExactIdx(t), testDigest); err != nil {
		t.Fatal(err)
	}
	da, _ := os.ReadFile(a)
	db, _ := os.ReadFile(b)
	if !bytes.Equal(da, db) {
		t.Fatal("two saves of the same index differ byte-for-byte")
	}
}

func TestExactRejectCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.idx")
	os.WriteFile(path, []byte("not an artifact"), 0o644)
	if _, _, err := artifact.LoadExact(path); err == nil {
		t.Fatal("want error for corrupt file")
	}
	// An ngram artifact is not an exact artifact (magic differs).
	os.WriteFile(path, []byte("KURNIDX1..."), 0o644)
	if _, _, err := artifact.LoadExact(path); err == nil {
		t.Fatal("want error for ngram magic")
	}
	// truncated: write a valid one, cut it in half
	good := filepath.Join(t.TempDir(), "good.idx")
	if err := artifact.SaveExact(good, buildExactIdx(t), testDigest); err != nil {
		t.Fatal(err)
	}
	// Reverse cross-magic: the ngram loader must reject an exact artifact.
	if _, _, err := artifact.Load(good); err == nil {
		t.Fatal("ngram Load accepted an exact artifact")
	}
	data, _ := os.ReadFile(good)
	for _, n := range []int{len(data) / 2, len(data) - 1} {
		os.WriteFile(path, data[:n], 0o644)
		if _, _, err := artifact.LoadExact(path); err == nil {
			t.Fatalf("want error for file truncated to %d/%d bytes", n, len(data))
		}
	}
	// trailing data: a valid artifact with extra bytes appended
	os.WriteFile(path, append(append([]byte{}, data...), 0xff), 0o644)
	if _, _, err := artifact.LoadExact(path); err == nil {
		t.Fatal("want error for trailing data")
	}
}

// TestExactRejectHostileHeader: crafted headers claiming absurd section sizes
// must be rejected by plausibility bounds against the file size BEFORE any
// large allocation happens.
func TestExactRejectHostileHeader(t *testing.T) {
	mk := func(hdr string) string {
		var buf bytes.Buffer
		buf.WriteString("KURNEXA1")
		var uv [binary.MaxVarintLen64]byte
		buf.Write(uv[:binary.PutUvarint(uv[:], uint64(len(hdr)))])
		buf.WriteString(hdr)
		path := filepath.Join(t.TempDir(), "hostile.idx")
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cases := []struct{ name, hdr string }{
		{"key_count", `{"key_count":50000000,"postings_len":50000000,"arena_len":50000000}`},
		{"arena_len", `{"key_count":1,"postings_len":1,"arena_len":1000000000}`},
		{"postings_len", `{"key_count":1,"postings_len":2000000000,"arena_len":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := artifact.LoadExact(mk(tc.hdr)); err == nil || !strings.Contains(err.Error(), "implausible") {
				t.Fatalf("want plausibility-bound error, got: %v", err)
			}
		})
	}
	// Negative and inconsistent counts must also be rejected (not implausible-
	// bounded, just invalid).
	for _, hdr := range []string{
		`{"key_count":-1,"postings_len":0,"arena_len":0}`,
		`{"key_count":1,"postings_len":-5,"arena_len":1}`,
		`{"key_count":1,"postings_len":1,"arena_len":-2}`,
		`{"key_count":2,"postings_len":1,"arena_len":2}`, // fewer postings than keys
		`{"key_count":2,"postings_len":2,"arena_len":1}`, // arena smaller than one byte per key
	} {
		if _, _, err := artifact.LoadExact(mk(hdr)); err == nil {
			t.Fatalf("header %s accepted", hdr)
		}
	}
}

// buildBigExactIdx builds a deterministic ~50k-key index for the benchmarks.
func buildBigExactIdx(tb testing.TB) *exact.Index {
	tb.Helper()
	rng := rand.New(rand.NewSource(7))
	const letters = "abcdefghijklmnopqrstuvwxyz"
	b := exact.NewBuilder()
	key := make([]byte, 24)
	for ord := uint32(0); ord < 50000; ord++ {
		for i := range key {
			key[i] = letters[rng.Intn(len(letters))]
		}
		b.Add(ord, []string{string(key)})
	}
	idx, err := b.Finish()
	if err != nil {
		tb.Fatalf("Finish: %v", err)
	}
	return idx
}

func BenchmarkExactSave(b *testing.B) {
	idx := buildBigExactIdx(b)
	dir := b.TempDir()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := artifact.SaveExact(filepath.Join(dir, fmt.Sprintf("big-%d.idx", i)), idx, testDigest); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExactLoad(b *testing.B) {
	idx := buildBigExactIdx(b)
	path := filepath.Join(b.TempDir(), "big.idx")
	if err := artifact.SaveExact(path, idx, testDigest); err != nil {
		b.Fatal(err)
	}
	if fi, err := os.Stat(path); err == nil {
		b.Logf("artifact size: %d bytes", fi.Size())
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := artifact.LoadExact(path); err != nil {
			b.Fatal(err)
		}
	}
}
