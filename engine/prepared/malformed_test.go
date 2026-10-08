package prepared

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"unsafe"
)

// withSection re-lays buf with the body of section kind replaced.
func withSection(buf []byte, kind uint32, body []byte) []byte {
	old := rawTable(buf)
	secs := make([]section, len(old))
	bodies := make([][]byte, len(old))
	for i, s := range old {
		secs[i] = section{kind: s.kind, flags: s.flags, length: s.length}
		bodies[i] = buf[s.off : s.off+s.length]
		if s.kind == kind {
			bodies[i] = body
			secs[i].length = uint64(len(body))
		}
	}
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

// editConfig and editBuild rewrite a JSON section through its own type, so
// the result stays canonical and only the intended check can refuse it.
func editConfig(buf []byte, edit func(d *configDoc)) []byte {
	var d configDoc
	if err := json.Unmarshal(body(buf, KindConfig), &d); err != nil {
		panic(err)
	}
	edit(&d)
	b, err := json.Marshal(&d)
	if err != nil {
		panic(err)
	}
	return withSection(buf, KindConfig, b)
}

func editBuild(buf []byte, edit func(bi *BuildInfo)) []byte {
	var bi BuildInfo
	if err := json.Unmarshal(body(buf, KindBuild), &bi); err != nil {
		panic(err)
	}
	edit(&bi)
	b, err := json.Marshal(&bi)
	if err != nil {
		panic(err)
	}
	return withSection(buf, KindBuild, b)
}

func entry(buf []byte, kind uint32) []byte {
	return buf[headerSize+rawFind(buf, kind).idx*tableEntrySize:]
}

func body(buf []byte, kind uint32) []byte {
	s := rawFind(buf, kind)
	return buf[s.off : s.off+s.length]
}

// ngramRow returns gram row i of the ngram section.
func ngramRow(buf []byte, i int) []byte { return body(buf, KindNgram)[8+32*i:] }

// bitmapBytes locates gram i's frozen bitmap inside buf.
func bitmapBytes(t *testing.T, buf []byte, i int) []byte {
	v, err := NewView(buf)
	if err != nil {
		t.Fatal(err)
	}
	fb := v.FrozenBitmap(i)
	off := int(uintptr(unsafe.Pointer(&fb[0])) - uintptr(unsafe.Pointer(&buf[0])))
	return buf[off : off+len(fb)]
}

// firstArrayBitmap returns the index of a gram whose first container is an
// array holding at least two values.
func firstArrayBitmap(t *testing.T, buf []byte) int {
	v, err := NewView(buf)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < v.GramCount(); i++ {
		b := v.FrozenBitmap(i)
		n := int(le32(b, len(b)-4) >> 15)
		types := b[len(b)-4-n:]
		counts := b[len(b)-4-3*n:]
		if n == 1 && types[0] == 2 && le16(counts, 0) >= 1 {
			return i
		}
	}
	t.Fatal("no multi-value array bitmap in fixture")
	return -1
}

func le16(b []byte, off int) uint16 { return uint16(b[off]) | uint16(b[off+1])<<8 }

type malformedCase struct {
	name   string
	mode   string
	mutate func(t *testing.T, buf []byte) []byte // returns the mutated buffer
	want   error                                 // ErrMalformed unless set
	raw    bool                                  // skip the rehash
}

func malformedCases() []malformedCase {
	sameLen := func(f func(t *testing.T, b []byte)) func(t *testing.T, buf []byte) []byte {
		return func(t *testing.T, buf []byte) []byte { b := clone(buf); f(t, b); return b }
	}
	return []malformedCase{
		// Header and table.
		{name: "bad magic", mutate: sameLen(func(t *testing.T, b []byte) { b[0] = 'X' })},
		{name: "unknown major version", want: ErrIncompatible, mutate: sameLen(func(t *testing.T, b []byte) { b[8] = 2 })},
		{name: "unknown header flags", want: ErrIncompatible, mutate: sameLen(func(t *testing.T, b []byte) { putLE32(b[12:], 1) })},
		{name: "zero section count", mutate: sameLen(func(t *testing.T, b []byte) { putLE32(b[16:], 0) })},
		{name: "huge section count", mutate: sameLen(func(t *testing.T, b []byte) { putLE32(b[16:], math.MaxUint32) })},
		{name: "header reserved nonzero", mutate: sameLen(func(t *testing.T, b []byte) { putLE32(b[20:], 1) })},
		{name: "table reserved nonzero", mutate: sameLen(func(t *testing.T, b []byte) { putLE64(entry(b, KindIDs)[56:], 1) })},
		{name: "unknown section flag", mutate: sameLen(func(t *testing.T, b []byte) { putLE32(entry(b, KindIDs)[4:], 3) })},
		{name: "section offset past file", mutate: sameLen(func(t *testing.T, b []byte) {
			putLE64(entry(b, KindIDs)[8:], uint64(len(b))+Align)
		})},
		{name: "misaligned section", mutate: sameLen(func(t *testing.T, b []byte) {
			e := entry(b, KindIDs)
			putLE64(e[8:], le64(e, 8)+8)
		})},
		{name: "overlapping sections", mutate: sameLen(func(t *testing.T, b []byte) {
			rec := rawFind(b, KindRecords)
			putLE64(entry(b, KindIDs)[8:], rec.off+Align)
		})},
		{name: "offsets not increasing", mutate: sameLen(func(t *testing.T, b []byte) {
			putLE64(entry(b, KindBuild)[8:], rawFind(b, KindConfig).off)
		})},
		{name: "length overflow", mutate: sameLen(func(t *testing.T, b []byte) {
			putLE64(entry(b, KindRecords)[16:], math.MaxUint64-16)
		})},
		{name: "last section past end", mutate: sameLen(func(t *testing.T, b []byte) {
			e := entry(b, KindNgram)
			putLE64(e[16:], le64(e, 16)+1)
		})},
		{name: "trailing bytes", mutate: func(t *testing.T, buf []byte) []byte {
			b := AlignedBuffer(len(buf) + Align)
			copy(b, buf)
			return b
		}},
		{name: "one trailing byte", mutate: func(t *testing.T, buf []byte) []byte {
			b := AlignedBuffer(len(buf) + 1)
			copy(b, buf)
			return b
		}},
		{name: "truncated", mutate: func(t *testing.T, buf []byte) []byte { return clone(buf[:len(buf)-1]) }},
		{name: "duplicate kinds", mutate: sameLen(func(t *testing.T, b []byte) { putLE32(entry(b, KindBuild)[0:], KindConfig) })},
		{name: "missing required section", mutate: sameLen(func(t *testing.T, b []byte) {
			e := entry(b, KindIDs)
			putLE32(e[0:], 99) // unknown, optional: ids now missing
			putLE32(e[4:], 0)
		})},
		{name: "unknown required section", want: ErrIncompatible, mutate: sameLen(func(t *testing.T, b []byte) {
			putLE32(entry(b, KindIDs)[0:], 99)
		})},
		{name: "index section disagrees with mode", mutate: sameLen(func(t *testing.T, b []byte) {
			putLE32(entry(b, KindNgram)[0:], KindExact)
		})},
		{name: "nonzero gap before first section", mutate: sameLen(func(t *testing.T, b []byte) {
			first := rawTable(b)[0].off
			hl := uint64(headerSize + len(rawTable(b))*tableEntrySize + digestSize)
			if first == hl {
				t.Skip("no header gap in this fixture")
			}
			b[first-1] = 1
		})},
		{name: "nonzero gap between sections", mutate: sameLen(func(t *testing.T, b []byte) {
			for _, s := range rawTable(b)[:len(rawTable(b))-1] {
				if s.end()%Align != 0 {
					b[s.end()] = 0x80
					return
				}
			}
			t.Fatal("no inter-section gap in fixture")
		})},

		// Config and build sections.
		{name: "config not canonical", mutate: func(t *testing.T, buf []byte) []byte {
			return withSection(buf, KindConfig, append([]byte(" "), body(buf, KindConfig)...))
		}},
		{name: "config unknown field", mutate: func(t *testing.T, buf []byte) []byte {
			return withSection(buf, KindConfig, append([]byte(`{"zz":1,`), body(buf, KindConfig)[1:]...))
		}},
		{name: "config bad list name", mutate: func(t *testing.T, buf []byte) []byte {
			return editConfig(buf, func(d *configDoc) { d.Name = "../etc" })
		}},
		{name: "config bad digest", mutate: func(t *testing.T, buf []byte) []byte {
			return editConfig(buf, func(d *configDoc) { d.ConfigDigest = strings.ToUpper(d.ConfigDigest) })
		}},
		{name: "config bad gram size", mutate: func(t *testing.T, buf []byte) []byte {
			return editConfig(buf, func(d *configDoc) {
				d.List = json.RawMessage(bytes.Replace(d.List, []byte(`"grams":[2,3]`), []byte(`"grams":[0,3]`), 1))
			})
		}},
		{name: "config unknown mode", mutate: func(t *testing.T, buf []byte) []byte {
			return editConfig(buf, func(d *configDoc) {
				d.List = json.RawMessage(bytes.Replace(d.List, []byte(`"mode":"ngram"`), []byte(`"mode":"fuzzy"`), 1))
			})
		}},
		{name: "build entries inconsistent", mutate: func(t *testing.T, buf []byte) []byte {
			return editBuild(buf, func(bi *BuildInfo) { bi.Entries++ })
		}},
		{name: "build keys inconsistent", mutate: func(t *testing.T, buf []byte) []byte {
			return editBuild(buf, func(bi *BuildInfo) { bi.Keys++ })
		}},
		{name: "build keyless over entries", mutate: func(t *testing.T, buf []byte) []byte {
			return editBuild(buf, func(bi *BuildInfo) { bi.KeylessEntries = bi.Entries + 1 })
		}},
		{name: "build negative count", mutate: func(t *testing.T, buf []byte) []byte {
			return editBuild(buf, func(bi *BuildInfo) { bi.DroppedKeys = -1 })
		}},
		{name: "build base id not hex", mutate: func(t *testing.T, buf []byte) []byte {
			return editBuild(buf, func(bi *BuildInfo) { bi.BaseID = "not-a-digest" })
		}},

		// Records.
		{name: "record count overflows", mutate: sameLen(func(t *testing.T, b []byte) { putLE64(body(b, KindRecords), 1<<61) })},
		{name: "record count max", mutate: sameLen(func(t *testing.T, b []byte) { putLE64(body(b, KindRecords), math.MaxUint64) })},
		{name: "record count off by one", mutate: sameLen(func(t *testing.T, b []byte) {
			r := body(b, KindRecords)
			putLE64(r, le64(r, 0)+1)
		})},
		{name: "record id offset not packed", mutate: sameLen(func(t *testing.T, b []byte) { putLE64(body(b, KindRecords)[8:], 1) })},
		{name: "record id length overruns", mutate: sameLen(func(t *testing.T, b []byte) { putLE64(body(b, KindRecords)[16:], math.MaxUint64) })},
		{name: "record empty id", mutate: sameLen(func(t *testing.T, b []byte) { putLE64(body(b, KindRecords)[16:], 0) })},
		{name: "record key count overruns", mutate: sameLen(func(t *testing.T, b []byte) { putLE64(body(b, KindRecords)[8+24:], 1<<40) })},
		{name: "record null flag with keys", mutate: sameLen(func(t *testing.T, b []byte) {
			r := body(b, KindRecords)
			for i := 0; ; i++ {
				row := r[8+48*i:]
				if kc := le64(row, 24); kc > 0 && kc&keysNullFlag == 0 {
					putLE64(row[24:], kc|keysNullFlag)
					return
				}
			}
		})},
		{name: "record key table count overflows", mutate: sameLen(func(t *testing.T, b []byte) {
			r := body(b, KindRecords)
			putLE64(r[8+48*le64(r, 0):], 1<<62)
		})},
		{name: "record payload length overruns", mutate: sameLen(func(t *testing.T, b []byte) {
			r := body(b, KindRecords)
			n := le64(r, 0)
			putLE64(r[8+48*(n-1)+40:], le64(r, int(8+48*(n-1)+40))+1)
		})},

		// IDs.
		{name: "ids count overflows", mutate: sameLen(func(t *testing.T, b []byte) { putLE64(body(b, KindIDs), 1<<62) })},
		{name: "ids unsorted", mutate: sameLen(func(t *testing.T, b []byte) {
			ids := body(b, KindIDs)
			a, c := le64(ids, 8), le64(ids, 16)
			putLE64(ids[8:], c)
			putLE64(ids[16:], a)
		})},
		{name: "ids duplicate ordinal", mutate: sameLen(func(t *testing.T, b []byte) {
			ids := body(b, KindIDs)
			putLE64(ids[16:], le64(ids, 8))
		})},
		{name: "ids ordinal out of range", mutate: sameLen(func(t *testing.T, b []byte) {
			ids := body(b, KindIDs)
			putLE64(ids[8:], le64(ids, 0))
		})},

		// Ngram section.
		{name: "gram count overflows", mutate: sameLen(func(t *testing.T, b []byte) { putLE64(body(b, KindNgram), 1<<60) })},
		{name: "gram count max", mutate: sameLen(func(t *testing.T, b []byte) { putLE64(body(b, KindNgram), math.MaxUint64) })},
		{name: "grams unsorted", mutate: sameLen(func(t *testing.T, b []byte) {
			ng := body(b, KindNgram)
			g := le64(ng, 0)
			ng[8+32*g+16] = 0xff // first byte of the first gram
		})},
		{name: "gram blob length overruns", mutate: sameLen(func(t *testing.T, b []byte) {
			ng := body(b, KindNgram)
			putLE64(ng[8+32*le64(ng, 0)+8:], math.MaxUint64)
		})},
		{name: "misaligned bitmap", mutate: sameLen(func(t *testing.T, b []byte) {
			r := ngramRow(b, 1)
			putLE64(r[16:], le64(r, 16)+8)
		})},
		{name: "bitmap outside blob", mutate: sameLen(func(t *testing.T, b []byte) {
			r := ngramRow(b, 0)
			putLE64(r[16:], 1<<40)
		})},
		{name: "bitmap length overruns", mutate: sameLen(func(t *testing.T, b []byte) {
			r := ngramRow(b, 0)
			putLE64(r[24:], math.MaxUint64-7)
		})},
		{name: "numOrds inconsistent", mutate: sameLen(func(t *testing.T, b []byte) {
			ng := body(b, KindNgram)
			at := 8 + 32*le64(ng, 0)
			putLE64(ng[at:], le64(ng, int(at))-1)
		})},
		{name: "numOrds over records", mutate: sameLen(func(t *testing.T, b []byte) {
			ng := body(b, KindNgram)
			putLE64(ng[8+32*le64(ng, 0):], 1<<33)
		})},
		{name: "nonzero bitmap padding", mutate: sameLen(func(t *testing.T, b []byte) {
			ng := body(b, KindNgram)
			g := int(le64(ng, 0))
			for i := 0; i+1 < g; i++ {
				r, n := ngramRow(b, i), ngramRow(b, i+1)
				if end := le64(r, 16) + le64(r, 24); end < le64(n, 16) {
					bm := bitmapBytes(t, b, i)
					base := int(uintptr(unsafe.Pointer(&bm[0])) - uintptr(unsafe.Pointer(&b[0])))
					b[base+len(bm)] = 1
					return
				}
			}
			t.Fatal("no bitmap padding in fixture")
		})},
		{name: "frozen cookie", mutate: sameLen(func(t *testing.T, b []byte) {
			bm := bitmapBytes(t, b, 0)
			bm[len(bm)-4] ^= 1
		})},
		{name: "frozen container count too large", mutate: sameLen(func(t *testing.T, b []byte) {
			bm := bitmapBytes(t, b, 0)
			putLE32(bm[len(bm)-4:], frozenCookie|(frozenMaxConts+1)<<15)
		})},
		{name: "frozen container count zero", mutate: sameLen(func(t *testing.T, b []byte) {
			bm := bitmapBytes(t, b, 0)
			putLE32(bm[len(bm)-4:], frozenCookie)
		})},
		{name: "frozen typecode unknown", mutate: sameLen(func(t *testing.T, b []byte) {
			bm := bitmapBytes(t, b, 0)
			bm[len(bm)-5] = 9
		})},
		{name: "frozen typecode changes implied length", mutate: sameLen(func(t *testing.T, b []byte) {
			bm := bitmapBytes(t, b, 0)
			bm[len(bm)-5] = 1 // array -> bitset: implies 8192 data bytes
		})},
		{name: "frozen count changes implied length", mutate: sameLen(func(t *testing.T, b []byte) {
			bm := bitmapBytes(t, b, 0)
			c := bm[len(bm)-4-3:]
			putLE16(c, le16(c, 0)+1)
		})},
		{name: "frozen run container empty", mutate: sameLen(func(t *testing.T, b []byte) {
			// An array retyped as an empty run container. (The exact
			// empty-run refusal, with a consistent length, is pinned in
			// frozen_test.go.)
			i := firstArrayBitmap(t, b)
			bm := bitmapBytes(t, b, i)
			putLE16(bm[len(bm)-4-3:], 0) // count
			bm[len(bm)-5] = 3            // run
		})},
		{name: "frozen array unsorted", mutate: sameLen(func(t *testing.T, b []byte) {
			i := firstArrayBitmap(t, b)
			bm := bitmapBytes(t, b, i)
			bm[0], bm[1], bm[2], bm[3] = bm[2], bm[3], bm[0], bm[1]
		})},
		{name: "frozen ordinal beyond numOrds", mutate: sameLen(func(t *testing.T, b []byte) {
			i := firstArrayBitmap(t, b)
			bm := bitmapBytes(t, b, i)
			n := int(le16(bm[len(bm)-4-3:], 0)) + 1
			putLE16(bm[2*(n-1):], 0xffff)
		})},

		// Exact section.
		{name: "exact key count overflows", mode: "exact", mutate: sameLen(func(t *testing.T, b []byte) { putLE64(body(b, KindExact), 1<<62) })},
		{name: "exact postings count overflows", mode: "exact", mutate: sameLen(func(t *testing.T, b []byte) { putLE64(body(b, KindExact)[8:], math.MaxUint64) })},
		{name: "exact arena length mismatch", mode: "exact", mutate: sameLen(func(t *testing.T, b []byte) {
			e := body(b, KindExact)
			putLE64(e[16:], le64(e, 16)-1)
		})},
		{name: "exact zero key length", mode: "exact", mutate: sameLen(func(t *testing.T, b []byte) { putLE32(body(b, KindExact)[24:], 0) })},
		{name: "exact run too long", mode: "exact", mutate: sameLen(func(t *testing.T, b []byte) {
			e := body(b, KindExact)
			putLE32(e[24+4*le64(e, 0):], 1<<20)
		})},
		{name: "exact keys unsorted", mode: "exact", mutate: sameLen(func(t *testing.T, b []byte) {
			e := body(b, KindExact)
			k, p := le64(e, 0), le64(e, 8)
			e[24+8*k+4*p] = 0xff // first arena byte
		})},
		{name: "exact ordinal out of range", mode: "exact", mutate: sameLen(func(t *testing.T, b []byte) {
			e := body(b, KindExact)
			putLE32(e[24+8*le64(e, 0):], 1<<30)
		})},
		{name: "exact run not ascending", mode: "exact", mutate: sameLen(func(t *testing.T, b []byte) {
			e := body(b, KindExact)
			k := le64(e, 0)
			var p uint64
			for i := uint64(0); i < k; i++ {
				rl := uint64(le32(e, int(24+4*k+4*i)))
				if rl >= 2 {
					at := 24 + 8*k + 4*p
					a, c := le32(e, int(at)), le32(e, int(at+4))
					putLE32(e[at:], c)
					putLE32(e[at+4:], a)
					return
				}
				p += rl
			}
			t.Fatal("no multi-posting run in fixture")
		})},
	}
}

func putLE16(b []byte, v uint16) { b[0], b[1] = byte(v), byte(v>>8) }

func TestMalformedButRehashed(t *testing.T) {
	fixtures := map[string][]byte{"ngram": artifactT(t, "ngram", 400), "exact": artifactT(t, "exact", 400)}
	for _, c := range malformedCases() {
		t.Run(strings.ReplaceAll(c.name, " ", "_"), func(t *testing.T) {
			mode := c.mode
			if mode == "" {
				mode = "ngram"
			}
			b := c.mutate(t, fixtures[mode])
			if !c.raw {
				rehash(b)
			}
			want := c.want
			if want == nil {
				want = ErrMalformed
			}
			_, _, err := VerifyBytes(b, LevelFull)
			if !errors.Is(err, want) {
				t.Fatalf("full verification: got %v, want %v", err, want)
			}
			t.Logf("refused: %v", err)
			if _, err := NewView(b); !errors.Is(err, want) {
				t.Fatalf("structure: got %v, want %v", err, want)
			}
		})
	}
}

// TestEveryPrefixAndByteFlip sweeps a small artifact: every truncation and
// every single-byte change (rehashed, so digests pass) must be handled
// without panicking; truncations must always fail.
func TestEveryPrefixAndByteFlip(t *testing.T) {
	for _, mode := range []string{"ngram", "exact"} {
		buf := artifactT(t, mode, 12)
		for n := 0; n < len(buf); n++ {
			if _, _, err := VerifyBytes(clone(buf[:n]), LevelFull); err == nil {
				t.Fatalf("%s: %d-byte prefix verified", mode, n)
			}
		}
		for i := 0; i < len(buf); i++ {
			for _, x := range []byte{0x01, 0x80, 0xff} {
				b := clone(buf)
				b[i] ^= x
				rehash(b)
				VerifyBytes(b, LevelFull)
				VerifyBytes(b, LevelStructure)
			}
		}
	}
}
