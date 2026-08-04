// Package ngram is a character-n-gram index over roaring-bitmap postings with
// an IDF-weighted pigeonhole lookup — the execution path that won the
// measured evaluation (naive map-counting, dense WAND, and DAAT/Block-Max-
// WAND all lost; only this path is kept).
package ngram

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/RoaringBitmap/roaring"
)

// Config fixes the index-time structure. Threshold/TopK are query-time.
type Config struct {
	Grams       []int
	StripSpaces bool // gram the space-stripped key: token-boundary robustness
}

// Index holds postings: gram -> roaring bitmap of entry ordinals. Immutable
// after Finish; safe for concurrent Lookup.
//
// Scratch memory: every concurrently active Lookup borrows a pooled dense
// accumulator of ~4 B × numOrds (float32 counts) plus a touched-ordinals
// slice sized by that query's generator hits. Scratches are pooled and
// reused, so steady-state cost is ~4 B × numOrds × peak Lookup concurrency
// (e.g. ~40 MB per concurrent query at 10M ordinals), not per call.
type Index struct {
	cfg      Config
	postings map[string]*roaring.Bitmap
	numOrds  uint32
	scratch  sync.Pool // *scratch: pooled dense accumulator
}

type scratch struct {
	counts  []float32
	touched []uint32
	// per-query gram-streaming buffers (O(query grams), tiny next to counts):
	offs   []int               // rune-start byte offsets of the query string
	seen   map[string]struct{} // cross-size gram dedup, cleared on put
	gi     []gramInfo          // query gram postings, rebuilt per lookup
	ordBuf []uint32            // ManyIterator batch buffer for the generator scan
}

// gramInfo is one distinct query gram's posting list with its IDF weight.
type gramInfo struct {
	bm   *roaring.Bitmap
	idf  float64
	card uint64
}

type Builder struct {
	idx  *Index
	gset map[string]struct{} // multi-key union scratch, reused across Adds
}

// NewBuilder returns a Builder for cfg. Gram sizes must be >= 1; a zero or
// negative n is a construction-time programming error (CharGrams would loop
// forever or return garbage), and NewBuilder panics on it with a descriptive
// message rather than growing an error return. engine.NewList validates the
// same constraint and returns an error, so config-driven callers get a clean
// failure before reaching this panic.
func NewBuilder(cfg Config) *Builder {
	for _, n := range cfg.Grams {
		if n < 1 {
			panic(fmt.Sprintf("ngram: NewBuilder: gram size %d is invalid (Grams must all be >= 1)", n))
		}
	}
	return &Builder{
		idx:  &Index{cfg: cfg, postings: map[string]*roaring.Bitmap{}},
		gset: map[string]struct{}{},
	}
}

// Add indexes the union of the grams of every analyzed key under ord.
// Nothing here depends on ordinal order (roaring Add and Finish's Maximum
// are order-insensitive) — callers conventionally add 0,1,2,… because
// ordinals index their entries slice, but that is their contract, not this
// package's (unlike exact.Builder.Add, whose ascending-ordinal contract IS
// load-bearing). An Add whose keys produce no grams is a no-op that still
// consumes the ordinal, keeping the caller's entries slice aligned.
func (b *Builder) Add(ord uint32, analyzedKeys []string) {
	if len(analyzedKeys) == 1 {
		// Fast path (the bulk-build common case: one key per entry): skip the
		// union set. CharGrams already dedups within one n, and grams of
		// different n can't collide (different lengths) except via the
		// short-key fallback (a key shorter than n indexes as itself for each
		// such n) — harmless, because Bitmap.Add is idempotent.
		for _, g := range b.idx.grams(analyzedKeys[0]) {
			b.addGram(g, ord)
		}
		return
	}
	clear(b.gset)
	for _, k := range analyzedKeys {
		for _, g := range b.idx.grams(k) {
			b.gset[g] = struct{}{}
		}
	}
	for g := range b.gset {
		b.addGram(g, ord)
	}
}

func (b *Builder) addGram(g string, ord uint32) {
	bm := b.idx.postings[g]
	if bm == nil {
		bm = roaring.New()
		b.idx.postings[g] = bm
	}
	bm.Add(ord)
}

// Finish freezes the index; the Builder must not be used afterwards.
func (b *Builder) Finish() *Index {
	// numOrds = highest ord + 1 across all postings (sizes the dense counter).
	var max uint32
	any := false
	for _, bm := range b.idx.postings {
		if bm.GetCardinality() > 0 {
			any = true
			if m := bm.Maximum(); m > max {
				max = m
			}
		}
	}
	if any {
		b.idx.numOrds = max + 1
	}
	idx := b.idx
	b.idx = nil // sever aliasing: a post-Finish Add fails fast instead of racing
	return idx
}

// Stats returns (ordinals covered, distinct grams).
func (i *Index) Stats() (int, int) { return int(i.numOrds), len(i.postings) }

// Cfg returns the index-time config (for serialization).
func (i *Index) Cfg() Config { return i.cfg }

// Postings exposes the postings map (serialization only — do not mutate).
func (i *Index) Postings() map[string]*roaring.Bitmap { return i.postings }

func (i *Index) NumOrds() uint32 { return i.numOrds }

// MaxGramSize bounds configured gram sizes. The floor matters for safety: a
// size < 1 makes CharGrams panic slicing (negative) or return [""] posting
// every ordinal to the empty gram (zero). The ceiling is a plausibility bound
// for untrusted artifact headers — real configs use 2-4, and a 16+-rune
// "gram" is a whole key, not a gram. Enforced at Restore (artifact trust
// boundary) and NewList (config trust boundary) so a config the engine
// accepts can never build an artifact it then refuses to load.
const MaxGramSize = 16

// Restore reconstructs an Index from deserialized parts. It returns an error
// (rather than the Index-only signature originally planned) because Lookup's
// dense accumulator indexes counts[ord] for 0..numOrds-1: a numOrds smaller
// than the true max ordinal in postings would make Lookup panic with
// index-out-of-range, so a corrupt or doctored header must be rejected here.
// Gram sizes are validated for the same reason: they come from the artifact
// header, and an out-of-range size panics or silently mis-indexes in
// CharGrams (see MaxGramSize).
func Restore(cfg Config, postings map[string]*roaring.Bitmap, numOrds uint32) (*Index, error) {
	if len(cfg.Grams) == 0 {
		return nil, fmt.Errorf("ngram: restore: no gram sizes")
	}
	for _, g := range cfg.Grams {
		if g < 1 || g > MaxGramSize {
			return nil, fmt.Errorf("ngram: restore: gram size %d out of range [1, %d]", g, MaxGramSize)
		}
	}
	for g, bm := range postings {
		if bm.GetCardinality() == 0 {
			continue
		}
		// numOrds <= m, not numOrds < m+1: m+1 wraps to 0 when m == MaxUint32,
		// which would accept any numOrds and let Lookup panic. The <= form is
		// equivalent without overflow, and correctly rejects m == MaxUint32
		// unconditionally (the required numOrds, 2^32, is not representable).
		if m := bm.Maximum(); numOrds <= m {
			return nil, fmt.Errorf("ngram: numOrds %d <= max ordinal %d (gram %q)", numOrds, m, g)
		}
	}
	return &Index{cfg: cfg, postings: postings, numOrds: numOrds}, nil
}

// grams returns the query/index gram set of an analyzed key.
func (i *Index) grams(analyzed string) []string {
	s := analyzed
	if i.cfg.StripSpaces {
		s = strings.ReplaceAll(s, " ", "")
	}
	if s == "" {
		return nil
	}
	var out []string
	for _, n := range i.cfg.Grams {
		out = append(out, CharGrams(s, n)...)
	}
	return out
}

// Hit is one matching ordinal with a 0..100 score (IDF coverage fraction).
type Hit struct {
	Ord   uint32
	Score float64
}

func (i *Index) getScratch() *scratch {
	if v := i.scratch.Get(); v != nil {
		return v.(*scratch)
	}
	return &scratch{
		counts: make([]float32, i.numOrds),
		seen:   map[string]struct{}{},
		ordBuf: make([]uint32, cancelCheckEvery),
	}
}

// scratchTrimCap: a pooled touched slice larger than this AND >4x the size
// this query needed is reallocated smaller on return, so one flood query
// (generators hitting a huge slice of the corpus) doesn't pin a worst-case
// slice in the pool forever. 1M ordinals = 4 MB.
const scratchTrimCap = 1 << 20

func (i *Index) putScratch(s *scratch) {
	used := len(s.touched)
	for _, ord := range s.touched {
		s.counts[ord] = 0
	}
	if c := cap(s.touched); c > scratchTrimCap && c > 4*used {
		s.touched = make([]uint32, 0, used)
	} else {
		s.touched = s.touched[:0]
	}
	// query-gram buffers are O(query length) — reset, no trim needed
	s.offs = s.offs[:0]
	s.gi = s.gi[:0]
	clear(s.seen)
	i.scratch.Put(s)
}

// Lookup runs the IDF-weighted pigeonhole scan: generators (rarest grams whose
// IDF mass exceeds maxScore-floor) are iterated into a dense accumulator; the
// remaining common grams are O(1) Contains-probed with early exit. Provably
// recall-complete for the threshold; results ranked score-desc, ord-asc, top-K.
// Score is IDF-weighted coverage of the query grams KNOWN to the index — grams
// absent from the corpus are excluded from the denominator (faithful to the
// reference; on tiny corpora this inflates scores for queries with unknown
// tokens).
func (i *Index) Lookup(analyzed string, threshold float64, topK int) []Hit {
	return i.LookupCtx(context.Background(), analyzed, threshold, topK)
}

// cancelCheckEvery is how many scan iterations pass between ctx checks:
// coarse enough to keep the hot loops branch-cheap, fine enough that a
// canceled 100ms+ scan stops within a fraction of a millisecond.
const cancelCheckEvery = 4096

// LookupCtx is Lookup with cooperative cancellation: the two scan loops
// (generator accumulation, candidate scoring) poll ctx every
// cancelCheckEvery iterations and return nil when it is done. A nil result
// is therefore ambiguous between "no hits" and "canceled" — callers that
// care check ctx.Err(). With context.Background the checks compile to a
// nil-channel comparison (free).
func (i *Index) LookupCtx(ctx context.Context, analyzed string, threshold float64, topK int) []Hit {
	if i.numOrds == 0 {
		return nil
	}
	s := analyzed
	if i.cfg.StripSpaces {
		s = strings.ReplaceAll(s, " ", "")
	}
	if s == "" {
		return nil
	}
	// IDF corpus size N is numOrds (max ordinal + 1) rather than a live-entry
	// count. Deliberate: numOrds is what survives artifact save/load (postings
	// carry no entry count), so scores reproduce exactly across build/restore. Do not "fix"
	// to a live-entry count — it would shift every IDF weight and break
	// score reproducibility against saved artifacts.
	n := float64(i.numOrds)
	sc := i.getScratch()
	defer i.putScratch(sc) // resets touched counts — safe on every early return
	var maxScore float64
	sc.offs = EachGram(s, i.cfg.Grams, sc.seen, sc.offs, func(g string) {
		bm := i.postings[g]
		if bm == nil {
			return
		}
		c := bm.GetCardinality()
		if c == 0 {
			return
		}
		idf := math.Log(1 + n/float64(c))
		sc.gi = append(sc.gi, gramInfo{bm, idf, c})
		maxScore += idf
	})
	gi := sc.gi
	if len(gi) == 0 || maxScore == 0 {
		return nil
	}
	floor := threshold * maxScore

	sort.Slice(gi, func(a, b int) bool { return gi[a].card < gi[b].card })
	needGenIDF := maxScore - floor
	var accIDF float64
	split := 0
	for split < len(gi) {
		accIDF += gi[split].idf
		split++
		if accIDF > needGenIDF {
			break
		}
	}
	gens, probes := gi[:split], gi[split:]
	var totalProbeIDF float64
	for _, p := range probes {
		totalProbeIDF += p.idf
	}

	done := ctx.Done() // nil for Background: the checks below cost one compare
	var sinceCheck int
	canceled := func() bool {
		sinceCheck++
		if done == nil || sinceCheck < cancelCheckEvery {
			return false
		}
		sinceCheck = 0
		select {
		case <-done:
			return true
		default:
			return false
		}
	}

	for _, g := range gens {
		// ManyIterator batch-fills a pooled buffer: same ordinals in the same
		// ascending order as Iterator/Next, minus the per-ordinal virtual call
		it := g.bm.ManyIterator()
		w := float32(g.idf)
		for {
			bn := it.NextMany(sc.ordBuf)
			if bn == 0 {
				break
			}
			for _, ord := range sc.ordBuf[:bn] {
				if canceled() {
					return nil
				}
				if sc.counts[ord] == 0 {
					sc.touched = append(sc.touched, ord)
				}
				sc.counts[ord] += w
			}
		}
	}

	out := make([]Hit, 0, len(sc.touched))
	for _, ord := range sc.touched {
		if canceled() {
			return nil
		}
		score := float64(sc.counts[ord])
		rem := totalProbeIDF
		for _, p := range probes {
			if score+rem < floor {
				break
			}
			rem -= p.idf
			if p.bm.Contains(ord) {
				score += p.idf
			}
		}
		if score >= floor {
			out = append(out, Hit{Ord: ord, Score: math.Round(score / maxScore * 100)})
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		return out[a].Ord < out[b].Ord
	})
	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// EachGram calls fn once per gram of s not yet in seen, for every size in
// sizes: left-to-right windows per size, sizes in order, a string shorter
// than a size contributing itself once (CharGrams' short-string rule). Grams
// are substrings of s — zero allocation per gram — produced by slicing s at
// rune-start byte offsets. seen accumulates across sizes (and across calls,
// if the caller wants a union); the caller owns clearing it. offs is a
// reusable offset buffer; the possibly-grown buffer is returned.
//
// The emission order equals iterating grams() output with a cross-size dedup
// set — the order LookupCtx and attribution consumed before streaming — so
// consumers see byte-identical sequences.
func EachGram(s string, sizes []int, seen map[string]struct{}, offs []int, fn func(g string)) []int {
	offs = offs[:0]
	for i := range s { // byte offsets of rune starts
		offs = append(offs, i)
	}
	offs = append(offs, len(s))
	rc := len(offs) - 1 // rune count
	for _, n := range sizes {
		if rc < n {
			if _, ok := seen[s]; !ok {
				seen[s] = struct{}{}
				fn(s)
			}
			continue
		}
		for j := 0; j+n <= rc; j++ {
			g := s[offs[j]:offs[j+n]]
			if _, ok := seen[g]; ok {
				continue
			}
			seen[g] = struct{}{}
			fn(g)
		}
	}
	return offs
}

// CharGrams returns the distinct character n-grams of s (s itself if shorter
// than n runes).
func CharGrams(s string, n int) []string {
	r := []rune(s)
	if len(r) < n {
		return []string{s}
	}
	out := make([]string, 0, len(r)-n+1)
	seen := map[string]struct{}{}
	for i := 0; i+n <= len(r); i++ {
		g := string(r[i : i+n])
		if _, ok := seen[g]; ok {
			continue
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}
	return out
}
