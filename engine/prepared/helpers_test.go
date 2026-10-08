package prepared

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine/exact"
	"github.com/kurn-dev/kurn/engine/ngram"
)

// These helpers build artifacts without importing package engine: the
// engine's prepared loader imports this package, so an in-package test
// that imported engine would be an import cycle. The builder package's
// tests cover the engine-backed path.

const (
	fakeAnalyzerDigest = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	fakeConfigDigest   = "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0"
)

// testRecords makes n records with a spread of key counts, awkward bytes
// and payload shapes (including none, JSON null and escaped characters).
func testRecords(n int, seed int64) []Record {
	rng := rand.New(rand.NewSource(seed))
	words := []string{"anna", "smith", "acme", "holdings", "zeta", "ltd", "müller", "o'brien", "<b>&co", "\u2028line", "jöhn", "doe", "alpha", "beta"}
	recs := make([]Record, n)
	for i := range recs {
		r := &recs[i]
		r.ID = fmt.Sprintf("id-%05d", (i*7919)%100000)
		nk := rng.Intn(4)
		for j := 0; j < nk; j++ {
			r.Keys = append(r.Keys, words[rng.Intn(len(words))]+" "+words[rng.Intn(len(words))])
		}
		if nk == 0 && i%2 == 0 {
			r.Keys = []string{} // empty, not null
		}
		switch i % 5 {
		case 0:
		case 1:
			r.Payload = []byte("null")
		case 2:
			r.Payload = mustCanon(fmt.Sprintf(`{"n":%d,"s":"<%s>&`+"\u2029"+`\"q\""}`, i, words[i%len(words)]))
		default:
			r.Payload = mustCanon(fmt.Sprintf(`{"program":["SDGT","X%d"],"meta":{"k":"%s"}}`, i%3, words[i%len(words)]))
		}
	}
	return recs
}

func mustCanon(s string) []byte {
	c, err := CanonicalPayload([]byte(s))
	if err != nil {
		panic(err)
	}
	return c
}

func normalize(k string) string { return strings.ToLower(strings.TrimSpace(k)) }

func buildNgram(recs []Record, grams []int, strip bool) *ngram.Index {
	b := ngram.NewBuilder(ngram.Config{Grams: grams, StripSpaces: strip})
	for i, r := range recs {
		var ks []string
		for _, k := range r.Keys {
			if a := normalize(k); a != "" {
				ks = append(ks, a)
			}
		}
		b.Add(uint32(i), ks)
	}
	return b.Finish()
}

func buildExact(recs []Record) *exact.Index {
	b := exact.NewBuilder()
	for i, r := range recs {
		var ks []string
		for _, k := range r.Keys {
			ks = append(ks, normalize(k))
		}
		b.Add(uint32(i), ks)
	}
	idx, err := b.Finish()
	if err != nil {
		panic(err)
	}
	return idx
}

func testContents(mode string, recs []Record) *Contents {
	c := &Contents{Name: "test-list", AnalyzerDigest: fakeAnalyzerDigest, ConfigDigest: fakeConfigDigest, Records: recs, Source: "unit"}
	switch mode {
	case "ngram":
		c.ListConfig = json.RawMessage(`{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"ngram","grams":[2,3],"threshold":0.6,"topk":100}}`)
		c.Ngram = buildNgram(recs, []int{2, 3}, false)
	case "exact":
		c.ListConfig = json.RawMessage(`{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"exact"}}`)
		c.Exact = buildExact(recs)
	default:
		panic(mode)
	}
	return c
}

func encodeT(t testing.TB, c *Contents) ([]byte, *Summary) {
	t.Helper()
	buf, sum, err := Encode(c)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return buf, sum
}

// artifactT returns a fresh encoded artifact of the given mode.
func artifactT(t testing.TB, mode string, n int) []byte {
	buf, _ := encodeT(t, testContents(mode, testRecords(n, 1)))
	return buf
}

// clone returns an aligned copy of b.
func clone(b []byte) []byte {
	c := AlignedBuffer(len(b))
	copy(c, b)
	return c
}

// rawSection is a table entry read without validation.
type rawSection struct {
	idx         int
	kind, flags uint32
	off, length uint64
}

func rawTable(buf []byte) []rawSection {
	n := int(binary.LittleEndian.Uint32(buf[16:]))
	out := make([]rawSection, n)
	for i := range out {
		e := buf[headerSize+i*tableEntrySize:]
		out[i] = rawSection{idx: i, kind: le32(e, 0), flags: le32(e, 4), off: le64(e, 8), length: le64(e, 16)}
	}
	return out
}

func rawFind(buf []byte, kind uint32) rawSection {
	for _, s := range rawTable(buf) {
		if s.kind == kind {
			return s
		}
	}
	panic(fmt.Sprintf("no section kind %d", kind))
}

// rehash recomputes every in-bounds section digest and then the header
// digest, so a mutation passes all hashing and only structure can catch it.
func rehash(buf []byte) {
	if len(buf) < headerSize {
		return
	}
	n := int(binary.LittleEndian.Uint32(buf[16:]))
	hl := headerSize + n*tableEntrySize + digestSize
	if n > MaxSections || hl > len(buf) {
		return
	}
	for i := 0; i < n; i++ {
		e := buf[headerSize+i*tableEntrySize:]
		off, l := le64(e, 8), le64(e, 16)
		if off <= uint64(len(buf)) && l <= uint64(len(buf))-off {
			sum := sha256.Sum256(buf[off : off+l])
			copy(e[24:56], sum[:])
		}
	}
	sum := sha256.Sum256(buf[:hl-digestSize])
	copy(buf[hl-digestSize:hl], sum[:])
}

func putLE64(b []byte, v uint64) { binary.LittleEndian.PutUint64(b, v) }
func putLE32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }

func (s rawSection) end() uint64 { return s.off + s.length }
