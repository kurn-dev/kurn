// Package exact is the normalized-exact index: analyzed key -> entry ordinals.
package exact

import (
	"fmt"
	"sort"
	"unsafe"
)

// Index maps analyzed keys to the ordinals of entries holding them.
//
// Memory layout (built by Finish, immutable after): every key's postings are
// one contiguous run inside the shared postings slice, and the map value is a
// packed (offset, length) locator. Compared with the naive map[string][]uint32
// this drops the per-key slice header and per-key backing allocation —
// dominant costs when almost every key has exactly one ordinal. Key bytes
// likewise share one contiguous arena instead of one heap allocation each.
//
// SAFETY INVARIANT (the package's one use of unsafe): the map's keys are
// unsafe.String views into arena. The arena is populated exactly once —
// written inside Finish, or handed to Restore whose caller relinquishes it —
// before any view escapes, and is never mutated afterwards. That makes the
// views immutable, satisfying Go's string contract.
type Index struct {
	m        map[string]packed
	arena    []byte   // every key's bytes, one contiguous block; write-once (Finish/Restore)
	postings []uint32 // all ordinals, each key's run contiguous, ascending within a run
	numOrds  uint32   // max ordinal + 1 (0 when empty); mirrors ngram.Index.NumOrds
}

// packed locates one key's run inside Index.postings: bits 20-63 hold the
// offset, bits 0-19 the run length. 44 offset bits address 16T postings (64TB
// of uint32 — unreachable in RAM, but Finish guards it anyway so the encoding
// is self-checking); 20 length bits cap one key at maxRun entries, guarded
// loudly in Finish.
//
// Deliberate deviation from the design sketch (bit 63 = single ordinal held
// inline): inlining singles would force Lookup to allocate a one-element
// slice per single-ordinal hit — the overwhelmingly common case. Keeping
// every run in postings keeps Lookup a zero-allocation subslice, at a cost of
// 4 bytes per single-ordinal key.
type packed uint64

const (
	lenBits = 20
	lenMask = 1<<lenBits - 1

	// maxRun is the longest postings run one key may hold (2^20-1). A list
	// where a million entries share one analyzed key is a data bug (e.g. an
	// analyzer normalizing everything to ""), so Finish rejects it loudly
	// with a KeyOverflowError rather than truncating silently.
	maxRun = lenMask

	// maxOff is the largest run offset the 44-bit field encodes.
	maxOff = 1<<44 - 1
)

// KeyOverflowError reports a single analyzed key held by more than maxRun
// entries: the 20-bit run-length field cannot represent it. This is a data
// bug (an analyzer collapsing every key to one value, or a genuinely
// pathological list), surfaced as a typed error so a hostile or broken input
// is REJECTED — never a panic mid-build, which would poison journals and
// block daemon startup (see engine store journal replay).
type KeyOverflowError struct {
	Key   string // the analyzed key
	Count int    // number of entries holding it
	Cap   int    // the encoding cap (maxRun)
}

func (e *KeyOverflowError) Error() string {
	return fmt.Sprintf("exact: key %q is held by %d entries, over the %d cap — a key shared by a million entries is a data bug (analyzer collapsing keys?)", e.Key, e.Count, e.Cap)
}

// Builder accumulates postings in a plain map; Finish compacts.
type Builder struct{ m map[string][]uint32 }

func NewBuilder() *Builder { return &Builder{m: map[string][]uint32{}} }

// Add indexes one entry's analyzed keys under its ordinal.
//
// Contract: Add is called at most once per ordinal, with strictly ascending
// ordinals across calls. That makes duplicate detection allocation-free: a
// key already holds this entry iff its postings tail equals ord.
func (b *Builder) Add(ord uint32, analyzedKeys []string) {
	for _, k := range analyzedKeys {
		if k == "" {
			continue
		}
		if p := b.m[k]; len(p) > 0 && p[len(p)-1] == ord {
			continue // duplicate key within this Add
		}
		b.m[k] = append(b.m[k], ord)
	}
}

// Finish compacts the builder map into the packed representation and freezes
// it; the Builder must not be used afterwards. A key held by more than
// maxRun entries is a data bug, not a programmer error: Finish returns a
// *KeyOverflowError for it (never a panic — this runs on untrusted list data
// inside a daemon).
func (b *Builder) Finish() (*Index, error) {
	src := b.m
	b.m = nil // sever aliasing: a post-Finish Add fails fast instead of racing
	totalOrds, totalKeyBytes := 0, 0
	for k, p := range src {
		if len(p) > maxRun {
			return nil, &KeyOverflowError{Key: k, Count: len(p), Cap: maxRun}
		}
		totalOrds += len(p)
		totalKeyBytes += len(k)
	}
	idx := &Index{
		m: make(map[string]packed, len(src)),
		// Sized exactly and never reallocated: the unsafe.String views below
		// point into this backing array, so it must not move.
		arena:    make([]byte, totalKeyBytes),
		postings: make([]uint32, 0, totalOrds),
	}
	off := 0
	for k, p := range src {
		pOff := len(idx.postings)
		if pOff > maxOff {
			return nil, fmt.Errorf("exact: postings offset %d exceeds the encodable maximum %d", pOff, maxOff)
		}
		idx.postings = append(idx.postings, p...)
		// Add's ascending-ordinal contract makes each run's tail its maximum.
		if last := p[len(p)-1]; last+1 > idx.numOrds {
			idx.numOrds = last + 1
		}
		n := copy(idx.arena[off:], k)
		// SAFETY: the arena region [off, off+n) was just written and is never
		// written again (see the invariant on Index) — the view is immutable.
		// len(k) >= 1 (Add skips empty keys), so &idx.arena[off] is in bounds.
		ak := unsafe.String(&idx.arena[off], n)
		off += n
		idx.m[ak] = packed(uint64(pOff)<<lenBits | uint64(len(p)))
	}
	return idx, nil
}

// Restore rebuilds an Index from its serialized sections (see
// kurn/engine/artifact): arena holds every key's bytes back to back, keyLens
// and runLens give each key's byte length and postings-run length in the same
// order, and postings holds the runs concatenated in that order. Restore
// takes ownership of arena and postings — the caller must never mutate them
// afterwards (the Index's map keys are views into arena; see the SAFETY
// INVARIANT on Index).
//
// Validation is structural: section lengths must agree exactly, every key and
// run must be non-empty, runs must fit the packed encoding, and keys must be
// distinct. Ordinal VALUES are not range-checked here — NumOrds reports the
// maximum seen, and the caller validates it against its entry count (a
// corrupt-but-well-formed payload can still return wrong ordinals, matching
// the ngram artifact's checksum-less corruption stance).
func Restore(arena []byte, keyLens, runLens []int, postings []uint32) (*Index, error) {
	if len(keyLens) != len(runLens) {
		return nil, fmt.Errorf("exact: restore: %d key lengths vs %d run lengths", len(keyLens), len(runLens))
	}
	idx := &Index{
		m:        make(map[string]packed, len(keyLens)),
		arena:    arena,
		postings: postings,
	}
	aOff, pOff := 0, 0
	for i := range keyLens {
		kl, rl := keyLens[i], runLens[i]
		if kl < 1 || kl > len(arena)-aOff {
			return nil, fmt.Errorf("exact: restore: key %d: length %d out of bounds (%d arena bytes left)", i, kl, len(arena)-aOff)
		}
		if rl < 1 || rl > maxRun {
			return nil, fmt.Errorf("exact: restore: key %d: run length %d out of range [1, %d]", i, rl, maxRun)
		}
		if rl > len(postings)-pOff {
			return nil, fmt.Errorf("exact: restore: key %d: run length %d exceeds the %d postings left", i, rl, len(postings)-pOff)
		}
		if pOff > maxOff {
			return nil, fmt.Errorf("exact: restore: postings offset %d exceeds the encodable maximum %d", pOff, maxOff)
		}
		// SAFETY: arena is owned by idx from here on and never written (see the
		// invariant on Index); kl >= 1 keeps &arena[aOff] in bounds.
		k := unsafe.String(&arena[aOff], kl)
		if _, dup := idx.m[k]; dup {
			return nil, fmt.Errorf("exact: restore: duplicate key %q", k)
		}
		idx.m[k] = packed(uint64(pOff)<<lenBits | uint64(rl))
		aOff += kl
		pOff += rl
	}
	if aOff != len(arena) {
		return nil, fmt.Errorf("exact: restore: key lengths sum to %d, arena is %d bytes", aOff, len(arena))
	}
	if pOff != len(postings) {
		return nil, fmt.Errorf("exact: restore: run lengths sum to %d, postings hold %d", pOff, len(postings))
	}
	for _, o := range postings {
		if o == ^uint32(0) {
			// o+1 would overflow numOrds to 0, defeating the caller's
			// max-ordinal-vs-entry-count validation.
			return nil, fmt.Errorf("exact: restore: ordinal %d out of range", o)
		}
		if o+1 > idx.numOrds {
			idx.numOrds = o + 1
		}
	}
	return idx, nil
}

// Lookup returns the ordinals of every entry holding the analyzed query as a
// key (ascending), nil on miss. The returned slice is shared with the index:
// writing to its elements would silently corrupt the index for every
// concurrent reader. Appending to it is safe — the full-cap subslice forces
// append to reallocate rather than clobber the next key's run. Scoring is the
// caller's business — an exact hit has no per-hit score of its own.
func (i *Index) Lookup(analyzed string) []uint32 {
	p, ok := i.m[analyzed]
	if !ok {
		return nil
	}
	off, n := uint64(p)>>lenBits, uint64(p)&lenMask
	return i.postings[off : off+n : off+n] // full-cap: appends can't clobber the next run
}

// Keys returns the number of distinct analyzed keys (stats).
func (i *Index) Keys() int { return len(i.m) }

// NumOrds returns max ordinal + 1 (0 when empty) — the minimum entry count an
// ordinal-aligned entries slice must have for every Lookup result to be a
// valid index into it. Same semantics as ngram.Index.NumOrds.
func (i *Index) NumOrds() uint32 { return i.numOrds }

// SortedKeys returns every analyzed key in sorted order. The strings are
// views into the index's arena (valid as long as the index is reachable);
// used to serialize the index deterministically.
func (i *Index) SortedKeys() []string {
	keys := make([]string, 0, len(i.m))
	for k := range i.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
