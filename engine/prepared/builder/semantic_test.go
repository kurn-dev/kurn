package builder_test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/RoaringBitmap/roaring"
	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/exact"
	"github.com/kurn-dev/kurn/engine/ngram"
	"github.com/kurn-dev/kurn/engine/prepared"
	"github.com/kurn-dev/kurn/engine/prepared/builder"
)

// semanticEntries are testEntries (which hold an empty and a null key
// list; the engine counts neither as a loss) plus a dropped key on an entry
// that keeps another key and an entry whose only key analyzes to nothing,
// so both derivable loss counters are nonzero.
func semanticEntries(n int, seed int64) []engine.Entry {
	e := testEntries(n, seed)
	e = append(e,
		engine.Entry{ID: "zz-dropped", Keys: []string{"   ", "Real Name"}},
		engine.Entry{ID: "zz-only-dropped", Keys: []string{"  "}},
	)
	return e
}

var semanticConfigs = []struct {
	name string
	cfg  func() engine.ListConfig
}{{"ngram", ngramConfig}, {"exact", exactConfig}, {"filtered", filteredConfig}}

var provenance = builder.Options{Source: "unit-test feed", CreatedAt: "2026-09-30T00:00:00Z", InputSHA256: strings.Repeat("cd", 32), Collapsed: 3}

func TestVerifySemanticClean(t *testing.T) {
	for _, tc := range semanticConfigs {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "a.kurn")
			res, err := builder.Write(path, "list-"+tc.name, tc.cfg(), semanticEntries(300, 5), provenance)
			if err != nil {
				t.Fatal(err)
			}
			r, err := builder.VerifySemantic(path)
			if err != nil {
				t.Fatal(err)
			}
			if !r.OK() || len(r.Failures) != 0 || !r.ConfigDigestOK || !r.AnalyzerDigestOK || !r.IndexOK || !r.CountersOK {
				t.Fatalf("clean artifact not OK: %+v", r)
			}
			wantKind := "ngram"
			if tc.name == "exact" {
				wantKind = "exact"
			}
			if r.IndexKind != wantKind || r.IndexMismatchOffset != -1 || r.IndexMismatch != "" {
				t.Fatalf("index %q offset %d %q", r.IndexKind, r.IndexMismatchOffset, r.IndexMismatch)
			}
			if r.Verified.Level != prepared.LevelFull || r.Verified.BaseID != res.BaseID || r.Verified.ContainerSHA256 != res.ContainerSHA256 {
				t.Fatalf("verified %+v, written %+v", r.Verified, res)
			}
			if r.RecomputedConfigDigest != res.ConfigDigest || r.RecomputedAnalyzerDigest != res.AnalyzerDigest {
				t.Fatalf("recomputed %s/%s, built %s/%s", r.RecomputedConfigDigest, r.RecomputedAnalyzerDigest, res.ConfigDigest, res.AnalyzerDigest)
			}
			if len(r.Counters) != 2 {
				t.Fatalf("counters %+v", r.Counters)
			}
			for _, c := range r.Counters {
				if !c.OK || c.Declared != c.Recomputed || c.Recomputed == 0 {
					t.Fatalf("counter %+v: want OK and nonzero", c)
				}
			}
			if len(r.Sections) != 5 {
				t.Fatalf("sections %+v", r.Sections)
			}
			for _, s := range r.Sections {
				if !s.OK || s.Offset != -1 || s.FileOffset != -1 {
					t.Fatalf("section %+v", s)
				}
			}
			d := r.Declared
			if d.Source != provenance.Source || d.CreatedAt != provenance.CreatedAt || d.InputSHA256 != provenance.InputSHA256 || d.Collapsed != provenance.Collapsed || d.UnknownSections != nil {
				t.Fatalf("declared %+v", d)
			}
		})
	}
}

// indexSpan returns the offset and length of the artifact's index section.
func indexSpan(t *testing.T, buf []byte) (off, length uint64) {
	t.Helper()
	n := int(binary.LittleEndian.Uint32(buf[16:]))
	for i := 0; i < n; i++ {
		e := buf[24+64*i:]
		if k := binary.LittleEndian.Uint32(e); k == prepared.KindNgram || k == prepared.KindExact {
			return binary.LittleEndian.Uint64(e[8:]), binary.LittleEndian.Uint64(e[16:])
		}
	}
	t.Fatal("no index section")
	return 0, 0
}

// TestVerifySemanticStaleDigest: a byte flipped in the index section with
// no digest recomputed is caught by full verification (the section
// digest), whose error comes back unchanged before any semantic step.
func TestVerifySemanticStaleDigest(t *testing.T) {
	for _, tc := range semanticConfigs {
		t.Run(tc.name, func(t *testing.T) {
			buf, _, _ := encode(t, "x", tc.cfg(), semanticEntries(200, 9))
			// Increment the last byte of the largest gram (ngram) or key
			// (exact): the section stays structurally valid, only its
			// digest goes stale.
			off, length := indexSpan(t, buf)
			at := off + length - 1 // exact: the arena ends the section
			if tc.name != "exact" {
				g := binary.LittleEndian.Uint64(buf[off:])
				head := off + 8 + 32*g
				at = head + 16 + binary.LittleEndian.Uint64(buf[head+8:]) - 1
			}
			buf[at]++
			if _, err := prepared.NewView(buf); err != nil {
				t.Fatalf("the flip is not structurally valid: %v", err)
			}
			_, _, want := prepared.VerifyBytes(buf, prepared.LevelFull)
			if !errors.Is(want, prepared.ErrIntegrity) || !strings.Contains(want.Error(), "digest mismatch") {
				t.Fatalf("LevelFull: %v", want)
			}
			r, err := builder.VerifySemanticBytes(buf)
			if r != nil {
				t.Fatalf("a report came back for a LevelFull failure: %+v", r)
			}
			if !errors.Is(err, prepared.ErrIntegrity) || err.Error() != want.Error() {
				t.Fatalf("got %v, want the LevelFull error %v", err, want)
			}
			if strings.Contains(err.Error(), "semantic") {
				t.Fatalf("a semantic step ran: %v", err)
			}
		})
	}
}

// rehashed encodes deliberately altered contents with prepared.Encode,
// which computes every digest itself, and checks that full verification
// accepts the result.
func rehashed(t *testing.T, c *prepared.Contents) []byte {
	t.Helper()
	buf, _, err := prepared.Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepared.VerifyBytes(buf, prepared.LevelFull); err != nil {
		t.Fatalf("the defect does not pass full verification: %v", err)
	}
	return buf
}

func cloneNgram(idx *ngram.Index) map[string]*roaring.Bitmap {
	post := make(map[string]*roaring.Bitmap, len(idx.Postings()))
	for g, bm := range idx.Postings() {
		post[g] = bm.Clone()
	}
	return post
}

func sortedGrams(post map[string]*roaring.Bitmap) []string {
	gs := make([]string, 0, len(post))
	for g := range post {
		gs = append(gs, g)
	}
	slices.Sort(gs)
	return gs
}

// exactParts flattens an exact index into what exact.Restore takes.
func exactParts(idx *exact.Index) (arena []byte, keyLens, runLens []int, postings []uint32, keys []string) {
	keys = idx.SortedKeys()
	for _, k := range keys {
		run := idx.Lookup(k)
		arena = append(arena, k...)
		keyLens = append(keyLens, len(k))
		runLens = append(runLens, len(run))
		postings = append(postings, run...)
	}
	return
}

type defect struct {
	name   string
	cfg    func() engine.ListConfig
	mutate func(t *testing.T, c *prepared.Contents) string // returns the expected failure text
	check  func(t *testing.T, r *builder.SemanticReport)
}

func semanticDefects() []defect {
	indexBad := func(t *testing.T, r *builder.SemanticReport) {
		if r.IndexOK || r.IndexMismatchOffset < 0 || !r.ConfigDigestOK || !r.AnalyzerDigestOK || !r.CountersOK {
			t.Fatalf("want only the index to fail: %+v", r)
		}
	}
	return []defect{
		{
			name: "a/ngram posting moved", cfg: ngramConfig,
			mutate: func(t *testing.T, c *prepared.Contents) string {
				post := cloneNgram(c.Ngram)
				for _, g := range sortedGrams(post) {
					bm := post[g]
					if bm.GetCardinality() < 2 {
						continue
					}
					x, hi := bm.Minimum(), bm.Maximum()
					for y := uint32(0); y < hi; y++ {
						if bm.Contains(y) {
							continue
						}
						// Move x to y: max, numOrds and the gram set stay
						// the same, so the section stays structurally valid.
						bm.Remove(x)
						bm.Add(y)
						idx, err := ngram.Restore(c.Ngram.Cfg(), post, c.Ngram.NumOrds())
						if err != nil {
							t.Fatal(err)
						}
						c.Ngram = idx
						d, inArtifact := min(x, y), y < x
						id := c.Records[d].ID
						if inArtifact {
							return fmt.Sprintf("gram %q: the artifact posts ordinal %d (record %q), the records do not", g, d, id)
						}
						return fmt.Sprintf("gram %q: the records post ordinal %d (record %q), the artifact does not", g, d, id)
					}
				}
				t.Fatal("no gram to alter")
				return ""
			},
			check: indexBad,
		},
		{
			name: "a/exact posting altered", cfg: exactConfig,
			mutate: func(t *testing.T, c *prepared.Contents) string {
				arena, kl, rl, postings, keys := exactParts(c.Exact)
				var pOff int
				for i, k := range keys {
					if rl[i] == 1 {
						old := postings[pOff]
						postings[pOff] = (old + 1) % uint32(len(c.Records))
						idx, err := exact.Restore(arena, kl, rl, postings)
						if err != nil {
							t.Fatal(err)
						}
						c.Exact = idx
						d, inArtifact := min(old, postings[pOff]), postings[pOff] < old
						id := c.Records[d].ID
						if inArtifact {
							return fmt.Sprintf("key %q: the artifact posts ordinal %d (record %q), the records do not", k, d, id)
						}
						return fmt.Sprintf("key %q: the records post ordinal %d (record %q), the artifact does not", k, d, id)
					}
					pOff += rl[i]
				}
				t.Fatal("no single-posting key")
				return ""
			},
			check: indexBad,
		},
		{
			name: "b/ngram gram removed", cfg: ngramConfig,
			mutate: func(t *testing.T, c *prepared.Contents) string {
				post := cloneNgram(c.Ngram)
				n := c.Ngram.NumOrds()
				for _, g := range sortedGrams(post) {
					// Keep numOrds = max ordinal + 1 of what remains.
					if post[g].Maximum()+1 < n {
						delete(post, g)
						idx, err := ngram.Restore(c.Ngram.Cfg(), post, n)
						if err != nil {
							t.Fatal(err)
						}
						c.Ngram = idx
						return fmt.Sprintf("gram %q is produced by the records, but missing from the artifact's index", g)
					}
				}
				t.Fatal("no removable gram")
				return ""
			},
			check: indexBad,
		},
		{
			name: "c/wrong config digest", cfg: filteredConfig,
			mutate: func(t *testing.T, c *prepared.Contents) string {
				c.ConfigDigest = strings.Repeat("0", 64)
				return "config digest: declared " + c.ConfigDigest
			},
			check: func(t *testing.T, r *builder.SemanticReport) {
				if r.ConfigDigestOK || !r.AnalyzerDigestOK || !r.IndexOK || !r.CountersOK || r.RecomputedConfigDigest == r.Verified.ConfigDigest {
					t.Fatalf("want only the config digest to fail: %+v", r)
				}
			},
		},
		{
			name: "d/wrong keyless_entries", cfg: ngramConfig,
			mutate: func(t *testing.T, c *prepared.Contents) string {
				c.KeylessEntries--
				return fmt.Sprintf("keyless_entries: declared %d, the records give %d", c.KeylessEntries, c.KeylessEntries+1)
			},
			check: func(t *testing.T, r *builder.SemanticReport) {
				if r.CountersOK || !r.IndexOK || !r.ConfigDigestOK || r.Counters[0].OK != true || r.Counters[1].OK {
					t.Fatalf("want only keyless_entries to fail: %+v", r)
				}
			},
		},
		{
			name: "d/wrong dropped_keys", cfg: exactConfig,
			mutate: func(t *testing.T, c *prepared.Contents) string {
				c.DroppedKeys += 7
				return fmt.Sprintf("dropped_keys: declared %d, the records give %d", c.DroppedKeys, c.DroppedKeys-7)
			},
			check: func(t *testing.T, r *builder.SemanticReport) {
				if r.CountersOK || !r.IndexOK || !r.ConfigDigestOK || r.Counters[0].OK || !r.Counters[1].OK {
					t.Fatalf("want only dropped_keys to fail: %+v", r)
				}
			},
		},
	}
}

// TestVerifySemanticRehashedDefects: each defect passes full verification
// (every digest and the base ID are consistent) and is refused by semantic
// verification with that specific difference named, and only it.
func TestVerifySemanticRehashedDefects(t *testing.T) {
	for _, d := range semanticDefects() {
		t.Run(d.name, func(t *testing.T) {
			c, err := builder.Prepare("x", d.cfg(), semanticEntries(250, 13), provenance)
			if err != nil {
				t.Fatal(err)
			}
			want := d.mutate(t, c)
			buf := rehashed(t, c)
			r, err := builder.VerifySemanticBytes(buf)
			if !errors.Is(err, prepared.ErrIntegrity) || r == nil {
				t.Fatalf("got %v (report %v), want an ErrIntegrity semantic mismatch", err, r != nil)
			}
			if r.OK() || len(r.Failures) != 1 || !strings.Contains(r.Failures[0], want) || !strings.Contains(err.Error(), want) {
				t.Fatalf("failures %q / error %v; want exactly one naming %q", r.Failures, err, want)
			}
			d.check(t, r)
			t.Logf("%s: %v", d.name, err)
		})
	}
}

// TestVerifySemanticEngineRefusesConfig: a configuration the engine cannot
// resolve is an incompatibility, not a semantic verdict.
func TestVerifySemanticEngineRefusesConfig(t *testing.T) {
	c, err := builder.Prepare("x", ngramConfig(), semanticEntries(20, 1), builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c.ListConfig = []byte(strings.Replace(string(c.ListConfig), `{`, `{"zz":1,`, 1))
	r, err := builder.VerifySemanticBytes(rehashed(t, c))
	if r != nil || !errors.Is(err, prepared.ErrIncompatible) {
		t.Fatalf("got %v (report %v), want ErrIncompatible", err, r)
	}
}

// TestVerifySemanticReportOwnsItsMemory: overwriting the buffer after the
// call leaves the report unchanged, for a passing and a failing report
// (whose descriptions quote grams and record IDs from the buffer).
func TestVerifySemanticReportOwnsItsMemory(t *testing.T) {
	clean, _, _ := encode(t, "x", ngramConfig(), semanticEntries(150, 21))
	c, err := builder.Prepare("x", ngramConfig(), semanticEntries(150, 21), provenance)
	if err != nil {
		t.Fatal(err)
	}
	semanticDefects()[0].mutate(t, c)
	bad := rehashed(t, c)
	for name, buf := range map[string][]byte{"clean": clean, "defect": bad} {
		r, err := builder.VerifySemanticBytes(buf)
		if (err == nil) != (name == "clean") || r == nil {
			t.Fatalf("%s: %v", name, err)
		}
		before := fmt.Sprintf("%#v", *r)
		for i := range buf {
			buf[i] = 0xa5
		}
		if after := fmt.Sprintf("%#v", *r); after != before {
			t.Fatalf("%s: the report changed when the buffer did:\n%s\n%s", name, before, after)
		}
	}
}
