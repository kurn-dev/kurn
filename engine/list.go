package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/kurn-dev/kurn/engine/analyzer"
	"github.com/kurn-dev/kurn/engine/exact"
	"github.com/kurn-dev/kurn/engine/ngram"
)

// QueryOpts are per-query overrides of the list's query-time defaults.
// Zero values mean "use the list default"; NEGATIVE values are the
// documented sentinel for "explicitly zero" — Threshold < 0 queries with no
// score floor, TopK < 0 returns every candidate. NaN Threshold is treated as
// the zero value (use the list default); callers accepting floating-point
// input should reject non-finite values at their boundary. (Zero itself can't
// carry the sentinel meaning without breaking zero-value-is-default.)
type QueryOpts struct {
	Threshold float64
	TopK      int
}

// segment is an immutable ordinal-aligned block of entries plus its index.
type segment struct {
	entries []Entry
	byID    map[string]uint32
	ng      *ngram.Index // nil in exact mode
	ex      *exact.Index // nil in ngram mode

	// Build-time loss counters (see List.BuildStats): keys the analyzer
	// collapsed to "" (dropped from the index), and entries whose keys ALL
	// collapsed — those still occupy an ordinal and count in Stats() entries
	// while being unfindable, so without these numbers the loss is invisible.
	droppedKeys    int
	keylessEntries int

	// unindexedEntries counts entries the segment holds but its index does
	// not reach: ReplaceWithIndex tolerates an index with FEWER ordinals
	// than entries (see there), and those entries are live, counted in
	// Stats(), and findable by nothing. Same invisible loss as a keyless
	// entry, different cause — an index stale against base.jsonl rather
	// than an analyzer that ate the keys — so it gets its own number
	// instead of muddying that one.
	unindexedEntries int

	// totalKeys counts the RAW keys of every entry in the segment (pre-
	// analysis, dropped ones included) — the basis for List.KeyCount and
	// the max_total_keys quota, which meters what the caller stores, not
	// what survives analysis.
	totalKeys int
}

// snapshot is the immutable state a query sees. Tombstones mask base entries
// (deleted, or superseded by an overlay upsert).
//
// Invariants that mutation code (Replace/Upsert/Delete/Compact) must uphold:
//   - tombstones ⊆ base.byID: never tombstone an ID absent from base;
//   - a live ID is never in both overlay and base un-tombstoned: an overlay
//     upsert of a base ID must tombstone the base copy;
//   - tombstones never apply to overlay entries.
type snapshot struct {
	base       *segment
	overlay    *segment
	tombstones map[string]struct{}
	version    string
}

// List is one named lookup list. All mutations swap a new snapshot; the query
// path is lock-free.
type List struct {
	name string
	cfg  ListConfig
	an   analyzer.Analyzer

	// cfgDigest is the identity of the RESOLVED configuration (defaults
	// applied, analyzer preset expanded to its installed step list) —
	// immutable, computed once in NewList. Store-managed version stamps
	// carry it: the same data bytes queried under a different mode,
	// analyzer, or default answer differently, so a stamp identifying
	// only the data would let two lists share a version while
	// contradicting each other.
	cfgDigest string
	snap      atomic.Pointer[snapshot]
	mu        sync.Mutex // serializes mutations (Replace/Upsert/Delete/Compact)
	gen       uint64     // snapshot generation; incremented under mu on every store

	// baseID is the base content's identity for version stamps (guarded by
	// mu). The Store sets it to a content hash of base.jsonl (or "empty"),
	// making versions restart-stable and content-addressed:
	// "<hash>@<entries>+j<bytes>.<jhash>+c<cfg>" — same disk state and
	// resolved configuration ⇒ same version.
	// Library-managed lists leave it "" and keep the process-local gen
	// format (documented in Version).
	baseID string

	// jhash is the running content hash of the journal's exact byte prefix
	// (guarded by mu; Store-managed lists only, nil otherwise). The byte
	// POSITION alone cannot be the overlay identity: two journals of equal
	// encoded length holding different mutations produce different answers,
	// and a version must never equate them. The Store seeds it from the
	// replayed prefix at open, extends it on every acknowledged append, and
	// resets it whenever the journal is truncated (Replace/Compact/create).
	jhash hash.Hash

	overlaySrc map[string]Entry // source entries for overlay rebuilds
}

// NewList validates config and returns an empty list.
// maxListTopK bounds a list's default topK. Far above any useful value —
// the server caps a request at 1000 — and low enough that topK plus the
// tombstone count cannot overflow the per-segment cut.
const maxListTopK = 1 << 20

func NewList(name string, cfg ListConfig) (*List, error) {
	// Sever every slice aliased with the caller FIRST: the config is
	// validated once and then trusted for the list's lifetime, so a caller
	// mutating its own ListConfig afterwards (or the view Config returns)
	// must not be able to reach past validation — a Grams entry flipped to
	// -1 through a shared backing array panicked gram iteration mid-query.
	cfg = cfg.clone()
	an, err := ResolveAnalyzer(cfg.Analyzer)
	if err != nil {
		return nil, err
	}
	// Threshold/TopK carry a NEGATIVE sentinel per QUERY (see QueryOpts),
	// never per list: a config is a set of defaults, and "default to no
	// floor at all" or "default to unlimited" are not defaults anyone
	// writes on purpose. Unvalidated, a hand-edited config.json loaded
	// straight into the maximal-scan, unlimited-collection shape, and a
	// near-MaxInt topk overflowed segK (topK + len(masked)) negative.
	// NaN slips through ordinary range comparisons (every one is false), so
	// it must be named: a NaN threshold would make every score comparison
	// false downstream — no floor, silently. HTTP JSON cannot express NaN;
	// this is the direct-library and config-decoder boundary.
	if math.IsNaN(cfg.Match.Threshold) || cfg.Match.Threshold < 0 || cfg.Match.Threshold > 1 {
		return nil, fmt.Errorf("list %s: match threshold %v out of range [0, 1] (0 means the mode default; the no-floor sentinel is per-query, not per-list)", name, cfg.Match.Threshold)
	}
	if cfg.Match.TopK < 0 || cfg.Match.TopK > maxListTopK {
		return nil, fmt.Errorf("list %s: match topk %d out of range [0, %d] (0 means the mode default; the unlimited sentinel is per-query, not per-list)", name, cfg.Match.TopK, maxListTopK)
	}
	switch cfg.Match.Mode {
	case "ngram":
		if len(cfg.Match.Grams) == 0 {
			cfg.Match.Grams = []int{2, 3}
		}
		// Validate here so config-driven callers get an error; ngram.NewBuilder
		// panics on the same condition (construction-time programming error).
		// Same bounds as ngram.Restore, so an accepted config can never build
		// an artifact the loader refuses.
		for _, g := range cfg.Match.Grams {
			if g < 1 || g > ngram.MaxGramSize {
				return nil, fmt.Errorf("list %s: gram size %d out of range [1, %d]", name, g, ngram.MaxGramSize)
			}
		}
		if cfg.Match.Threshold == 0 {
			cfg.Match.Threshold = 0.6
		}
		if cfg.Match.TopK == 0 {
			cfg.Match.TopK = 100
		}
	case "exact":
	default:
		return nil, fmt.Errorf("list %s: unknown match mode %q", name, cfg.Match.Mode)
	}
	switch cfg.Match.Fallback {
	case "":
	case "parent_domain":
		if cfg.Match.Mode != "exact" {
			return nil, fmt.Errorf("list %s: match fallback %q requires mode \"exact\", have %q", name, cfg.Match.Fallback, cfg.Match.Mode)
		}
	default:
		return nil, fmt.Errorf("list %s: unknown match fallback %q (valid: \"parent_domain\")", name, cfg.Match.Fallback)
	}
	if cfg.OverlayAutoCompact < 0 {
		return nil, fmt.Errorf("list %s: overlay_auto_compact %d is negative (0 disables)", name, cfg.OverlayAutoCompact)
	}
	for i, p := range cfg.Golden {
		switch {
		case p.Q == "":
			return nil, fmt.Errorf("list %s: golden[%d]: q must be non-empty", name, i)
		case p.ExpectID != "" && p.Absent:
			return nil, fmt.Errorf("list %s: golden[%d]: expect_id and absent are mutually exclusive", name, i)
		case p.ExpectID == "" && !p.Absent:
			return nil, fmt.Errorf("list %s: golden[%d]: one of expect_id or absent is required", name, i)
		case p.Absent && p.MinScore != 0:
			return nil, fmt.Errorf("list %s: golden[%d]: min_score requires expect_id", name, i)
		case math.IsNaN(p.MinScore) || p.MinScore < 0 || p.MinScore > 100:
			return nil, fmt.Errorf("list %s: golden[%d]: min_score %v out of range [0, 100]", name, i, p.MinScore)
		}
	}
	l := &List{name: name, cfg: cfg, an: an, cfgDigest: resolvedConfigDigest(cfg, an), overlaySrc: map[string]Entry{}}
	l.snap.Store(&snapshot{tombstones: map[string]struct{}{}, version: "empty@0"})
	return l, nil
}

// resolvedConfigDigest hashes what is actually INSTALLED: the config with
// defaults applied plus the analyzer's resolved step spec — a preset name
// hashes as the steps it expanded to, so a stamp made today can be compared
// with one made after a hypothetical preset redefinition. Domain-separated
// from the base and journal hashes; every component is length-prefixed
// (steps contain arbitrary text, and a collision here would let two
// different configurations share a version stamp — the exact defect the
// digest exists to prevent).
func resolvedConfigDigest(cfg ListConfig, an analyzer.Analyzer) string {
	h := sha256.New()
	h.Write([]byte("kurn config v1\x00"))
	var lb [8]byte
	write := func(b []byte) {
		binary.BigEndian.PutUint64(lb[:], uint64(len(b)))
		h.Write(lb[:])
		h.Write(b)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		// Unreachable: ListConfig is plain data (strings, numbers, bools,
		// slices of the same). Failing loud beats a silent wrong identity.
		panic(fmt.Sprintf("engine: marshaling ListConfig for its digest: %v", err))
	}
	write(raw)
	for _, s := range an.Steps() {
		write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ResolveAnalyzer turns an AnalyzerConfig (preset or steps) into an Analyzer.
func ResolveAnalyzer(cfg AnalyzerConfig) (analyzer.Analyzer, error) {
	if cfg.Preset != "" {
		return analyzer.Preset(cfg.Preset)
	}
	return analyzer.New(cfg.Steps)
}

func (l *List) Name() string { return l.name }

// Config returns the list's resolved configuration as a detached copy:
// slice-backed fields are cloned in both directions (see NewList), so
// neither mutating the returned value nor the caller's original can alter
// — or race with — the immutable config live queries read.
func (l *List) Config() ListConfig { return l.cfg.clone() }

// buildSegment analyzes and indexes entries (ordinal = slice position).
func (l *List) buildSegment(entries []Entry) (*segment, error) {
	seg := &segment{entries: entries, byID: make(map[string]uint32, len(entries))}
	var ngb *ngram.Builder
	var exb *exact.Builder
	if l.cfg.Match.Mode == "ngram" {
		ngb = ngram.NewBuilder(ngram.Config{Grams: l.cfg.Match.Grams, StripSpaces: l.cfg.Match.StripSpaces})
	} else {
		exb = exact.NewBuilder()
	}
	for i := range entries {
		ord := uint32(i)
		seg.byID[entries[i].ID] = ord
		keys := make([]string, 0, len(entries[i].Keys))
		for _, k := range entries[i].Keys {
			if a := l.an.Normalize(k); a != "" {
				keys = append(keys, a)
			} else {
				seg.droppedKeys++
			}
		}
		if len(keys) == 0 && len(entries[i].Keys) > 0 {
			seg.keylessEntries++ // occupies an ordinal but can never match
		}
		seg.totalKeys += len(entries[i].Keys)
		if ngb != nil {
			ngb.Add(ord, keys)
		} else {
			exb.Add(ord, keys)
		}
	}
	if ngb != nil {
		seg.ng = ngb.Finish()
	} else {
		ex, err := exb.Finish()
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", l.name, err)
		}
		seg.ex = ex
	}
	return seg, nil
}

// Replace atomically replaces the whole list content (new base, no overlay).
// Duplicate IDs within the batch are deduped: later duplicates win, matching
// upsert semantics. Replace takes ownership of the entries slice; the caller
// must not mutate it afterwards. A build-time data error (e.g. an exact-mode
// key held by more entries than the index encodes) is returned before
// anything is installed — the list is left untouched.
func (l *List) Replace(entries []Entry) error {
	_, seg, err := l.prepareBase(entries)
	if err != nil {
		return err
	}
	l.installBase(seg)
	return nil
}

// prepareBase dedupes entries (last-wins) and builds their base segment — the
// expensive half of Replace, done ONCE. Pure with respect to list state
// (buildSegment reads only the immutable cfg/analyzer), so it needs no lock.
// Package-internal fast path: Store uses the returned deduped entries for
// base.jsonl and the segment's index for base.idx, so disk and memory are
// built from the same slice with no second dedupe or discarded byID map.
func (l *List) prepareBase(entries []Entry) ([]Entry, *segment, error) {
	entries = dedupeLastWins(entries)
	seg, err := l.buildSegment(entries)
	if err != nil {
		return nil, nil, err
	}
	return entries, seg, nil
}

// installBase stores seg as the entire list content: no overlay, no
// tombstones — the cheap tail half of Replace for a segment produced by
// prepareBase (or assembled around a prebuilt index by ReplaceWithIndex).
func (l *List) installBase(seg *segment) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.overlaySrc = map[string]Entry{}
	l.gen++
	l.snap.Store(&snapshot{
		base:       seg,
		tombstones: map[string]struct{}{},
		version:    l.baseStampLocked(len(seg.entries)),
	})
}

// baseStampLocked is the version for a fresh base with an empty journal.
// Caller holds l.mu.
func (l *List) baseStampLocked(n int) string {
	if l.baseID != "" {
		return fmt.Sprintf("%s@%d+j0+c%s", l.baseID, n, l.cfgDigest)
	}
	return fmt.Sprintf("gen%d-base@%d", l.gen, n)
}

// stampFresh restamps a virgin empty list with the content-addressed form.
// NewList stamps "empty@0" before any Store identity exists; once the Store
// declares the base identity, the served stamp must carry the full form —
// including the config digest — and must match what a restart of the same
// empty list reproduces.
func (l *List) stampFresh() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.snap.Store(&snapshot{tombstones: map[string]struct{}{}, version: l.baseStampLocked(0)})
}

// SetBaseID declares the identity of the base content about to be installed
// (a content hash of base.jsonl, or "empty"): subsequent snapshot versions
// use the content-addressed format instead of process-local gen counters.
// Store-managed lists call this before installBase / journal replay; direct
// library users normally never do.
func (l *List) SetBaseID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.baseID = id
}

// setJournalHash installs the running journal content hash (see the jhash
// field). The Store hands over a hash already covering the journal's current
// byte prefix; appendJournal extends it under l.mu as records land.
func (l *List) setJournalHash(h hash.Hash) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.jhash = h
}

// BaseIDForArtifact returns the declared base content identity ("" for
// library-managed lists). saveArtifact records it so a reopen can refuse an
// index whose ordinals were assigned against different base content.
func (l *List) BaseIDForArtifact() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.baseID
}

// compactBuild is a prepared fold: the live entry set and its built segment,
// not yet installed. Store.Compact prepares, persists the fold to disk, and
// only then commits — a failed persist leaves the list untouched (no folded
// scores served that disk doesn't have).
type compactBuild struct {
	live []Entry
	seg  *segment
}

// PrepareCompact builds the folded base (base+overlay−tombstones) without
// touching list state. A build error (exact-mode hot key in the fold) leaves
// the list untouched.
func (l *List) PrepareCompact() (*compactBuild, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	live := l.liveEntriesLocked()
	seg, err := l.buildSegment(live)
	if err != nil {
		return nil, err
	}
	return &compactBuild{live: live, seg: seg}, nil
}

// CommitCompact installs a prepared fold as the entire list content with its
// persisted base identity (the hash persistBase computed): fresh base, empty
// overlay/tombstones, version "<id>@<n>+j0+c<cfg>". Store-only counterpart of the
// library's Compact; the Store's per-list mutation lock spans
// prepare→persist→commit, so no mutation can interleave.
func (l *List) CommitCompact(b *compactBuild, id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gen++
	l.baseID = id
	l.snap.Store(&snapshot{
		base:       b.seg,
		tombstones: map[string]struct{}{},
		version:    fmt.Sprintf("%s@%d+j0+c%s", id, len(b.live), l.cfgDigest),
	})
	l.overlaySrc = map[string]Entry{}
}

// ReplaceWithIndex is Replace with a prebuilt base index (e.g. loaded from an
// artifact): it skips analysis/index building, rebuilds byID from entries, and
// installs idx as the base segment's ngram index. Entries are deduped
// last-wins first (matching Replace), THEN validated against the index:
// artifacts are saved from already-deduped entries, so a post-dedupe length
// shorter than idx.NumOrds() means the base.jsonl/base.idx pair is mismatched
// and must be rejected here — index ordinals pointing past the entries slice
// would otherwise panic later in Query. NumOrds < len(entries) is tolerated
// (extra entries are simply unreachable via the index), which keeps a stale-
// index crash window recoverable instead of fatal. Takes ownership of entries
// like Replace.
func (l *List) ReplaceWithIndex(entries []Entry, idx *ngram.Index) error {
	return l.ReplaceWithIndexInfo(entries, idx, nil)
}

// ReplaceWithIndexInfo is ReplaceWithIndex carrying the index's build info
// (see IndexBuildInfo). Pass nil when it is genuinely unknown: the loss
// counters then read zero rather than being inferred from the index, which
// cannot distinguish a stale artifact from entries that analyzed to nothing.
func (l *List) ReplaceWithIndexInfo(entries []Entry, idx *ngram.Index, bi *IndexBuildInfo) error {
	if l.cfg.Match.Mode != "ngram" {
		return fmt.Errorf("list %s: ReplaceWithIndex requires ngram mode, have %q", l.name, l.cfg.Match.Mode)
	}
	if idx == nil {
		return fmt.Errorf("list %s: ReplaceWithIndex: nil index", l.name)
	}
	entries = dedupeLastWins(entries)
	if n := idx.NumOrds(); int(n) > len(entries) {
		return fmt.Errorf("list %s: index has %d ordinals but only %d entries", l.name, n, len(entries))
	}
	seg := &segment{entries: entries, byID: make(map[string]uint32, len(entries)), ng: idx}
	if err := seg.applyBuildInfo(bi, len(entries), idx.NumOrds()); err != nil {
		return fmt.Errorf("list %s: %s", l.name, err)
	}
	for i := range entries {
		seg.byID[entries[i].ID] = uint32(i)
		seg.totalKeys += len(entries[i].Keys)
	}
	l.installBase(seg)
	return nil
}

// ReplaceWithExactIndex is ReplaceWithIndex's exact-mode sibling: Replace
// with a prebuilt exact index (e.g. loaded from an artifact), skipping
// analysis/index building. Same dedupe-then-validate order and the same
// stale-index rationale: NumOrds > len(entries) means the base.jsonl/base.idx
// pair is mismatched (index ordinals would run past the entries slice and
// panic later in Query), while NumOrds < len(entries) is tolerated. Takes
// ownership of entries like Replace.
func (l *List) ReplaceWithExactIndex(entries []Entry, idx *exact.Index) error {
	return l.ReplaceWithExactIndexInfo(entries, idx, nil)
}

// ReplaceWithExactIndexInfo is ReplaceWithExactIndex carrying build info; see
// ReplaceWithIndexInfo.
func (l *List) ReplaceWithExactIndexInfo(entries []Entry, idx *exact.Index, bi *IndexBuildInfo) error {
	if l.cfg.Match.Mode != "exact" {
		return fmt.Errorf("list %s: ReplaceWithExactIndex requires exact mode, have %q", l.name, l.cfg.Match.Mode)
	}
	if idx == nil {
		return fmt.Errorf("list %s: ReplaceWithExactIndex: nil index", l.name)
	}
	entries = dedupeLastWins(entries)
	if n := idx.NumOrds(); int(n) > len(entries) {
		return fmt.Errorf("list %s: index has %d ordinals but only %d entries", l.name, n, len(entries))
	}
	seg := &segment{entries: entries, byID: make(map[string]uint32, len(entries)), ex: idx}
	if err := seg.applyBuildInfo(bi, len(entries), idx.NumOrds()); err != nil {
		return fmt.Errorf("list %s: %s", l.name, err)
	}
	for i := range entries {
		seg.byID[entries[i].ID] = uint32(i)
		seg.totalKeys += len(entries[i].Keys)
	}
	l.installBase(seg)
	return nil
}

// BaseNgram returns the current base segment's ngram index — nil for exact
// mode or when there is no base. Used to persist the base index (artifact
// save) without rebuilding it.
func (l *List) BaseNgram() *ngram.Index {
	s := l.snap.Load()
	if s.base == nil {
		return nil
	}
	return s.base.ng
}

// BaseExact returns the current base segment's exact index — nil for ngram
// mode or when there is no base. Same artifact-save role as BaseNgram.
func (l *List) BaseExact() *exact.Index {
	s := l.snap.Load()
	if s.base == nil {
		return nil
	}
	return s.base.ex
}

// LiveEntries returns the live entry set (base − tombstones + overlay),
// ID-sorted. The slice is fresh but shares Entry key/payload slices with the
// list; callers must treat them as read-only.
func (l *List) LiveEntries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.liveEntriesLocked()
}

// dedupeLastWins drops all but the last occurrence of each ID, preserving the
// relative order of the surviving occurrences.
func dedupeLastWins(entries []Entry) []Entry {
	last := make(map[string]int, len(entries))
	dup := false
	for i := range entries {
		if _, seen := last[entries[i].ID]; seen {
			dup = true
		}
		last[entries[i].ID] = i
	}
	if !dup {
		return entries
	}
	out := make([]Entry, 0, len(last))
	for i := range entries {
		if last[entries[i].ID] == i {
			out = append(out, entries[i])
		}
	}
	return out
}

// Upsert adds or replaces entries. Rebuilds the (small) overlay and swaps —
// changes are visible to the next query. Duplicate IDs within the batch are
// deduped last-wins, matching Replace semantics. Upsert retains the passed
// Entry values (Keys/Payload slices) in overlaySrc and future snapshots; the
// caller must not mutate them after the call (same convention as Replace).
// A build-time data error (e.g. an exact-mode hot key over the encoding cap)
// is returned BEFORE any state changes — the list is left exactly as it was.
func (l *List) Upsert(entries []Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, err := l.prepareUpsertLocked(entries)
	if err != nil {
		return err
	}
	l.commitOverlayLocked(b)
	return nil
}

// Delete tombstones an entry (base) and/or removes it from the overlay.
// Deleting an ID absent from both is a no-op (beyond a snapshot swap).
// Like Upsert, a build error leaves the list untouched.
func (l *List) Delete(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, err := l.prepareDeleteLocked(id)
	if err != nil {
		return err
	}
	l.commitOverlayLocked(b)
	return nil
}

// overlayBuild is a fully prepared overlay mutation: the candidate overlay
// source, tombstone set, and the already-built overlay segment. Splitting
// prepare from commit lets Store run its fallible work FIRST and journal
// only mutations known to be installable — an unbuildable overlay (exact-
// mode hot key) is never persisted, so replay can never be poisoned by a
// journaled-but-uninstallable batch.
type overlayBuild struct {
	src  map[string]Entry
	tomb map[string]struct{}
	seg  *segment
}

// prepareOverlayLocked computes the candidate overlay state after applying
// mutate to CLONES of the current overlaySrc/tombstones, and builds the
// overlay segment. Nothing reachable from the published snapshot (or from
// l.overlaySrc) is mutated; on error the list is exactly as before.
// Caller holds l.mu.
func (l *List) prepareOverlayLocked(mutate func(src map[string]Entry, tomb map[string]struct{}, base *segment)) (*overlayBuild, error) {
	s := l.snap.Load()
	src := make(map[string]Entry, len(l.overlaySrc))
	for k, v := range l.overlaySrc {
		src[k] = v
	}
	tomb := cloneSet(s.tombstones)
	mutate(src, tomb, s.base)
	entries := make([]Entry, 0, len(src))
	for _, e := range src {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].ID < entries[b].ID })
	var ov *segment
	if len(entries) > 0 {
		var err error
		ov, err = l.buildSegment(entries)
		if err != nil {
			return nil, err
		}
	}
	return &overlayBuild{src: src, tomb: tomb, seg: ov}, nil
}

// commitOverlayLocked installs a prepared overlay build: overlaySrc and the
// snapshot swap in one step. Never fails — the fallible work happened in
// prepareOverlayLocked. Caller holds l.mu. Library path: process-local gen
// stamp (jpos unknowable without a journal).
func (l *List) commitOverlayLocked(b *overlayBuild) {
	l.commitOverlayLockedAt(b, -1)
}

// commitOverlayLockedAt is commitOverlayLocked with a journal byte position
// for the version stamp: the Store passes the journal size after the append
// that persisted this mutation, yielding the restart-stable
// "<baseID>@<baseN>+j<jpos>.<jhash>+c<cfg>" form. The byte position is diagnostics
// (how much journal to replay); the CONTENT hash is the identity — equal
// positions with different journal bytes are different data and must never
// share a version. jpos < 0, an unset baseID, or a missing journal hash
// falls back to the gen format (never to a length-only stamp, which would
// claim a content identity it doesn't have). Caller holds l.mu.
func (l *List) commitOverlayLockedAt(b *overlayBuild, jpos int64) {
	prev := l.snap.Load()
	l.overlaySrc = b.src
	baseN := 0
	if prev.base != nil {
		baseN = len(prev.base.entries)
	}
	l.gen++
	version := fmt.Sprintf("gen%d-base@%d+ov%d-t%d", l.gen, baseN, len(b.src), len(b.tomb))
	if l.baseID != "" && jpos >= 0 && (jpos == 0 || l.jhash != nil) {
		version = fmt.Sprintf("%s@%d+j%d", l.baseID, baseN, jpos)
		if jpos > 0 {
			version += "." + baseIDFromHash(l.jhash)
		}
		version += "+c" + l.cfgDigest
	}
	l.snap.Store(&snapshot{
		base:       prev.base,
		overlay:    b.seg,
		tombstones: b.tomb,
		version:    version,
	})
}

// prepareUpsertLocked / prepareDeleteLocked are the Store-facing halves of
// Upsert/Delete: Store prepares (fallible), journals, then commits
// (infallible), so a journal append never precedes an unbuildable mutation.
// Direct List users get the same safety from Upsert/Delete's single lock
// hold. Mixing Store-managed lists with direct List mutation was already
// unsupported (direct mutations bypass the journal); with the split it can
// additionally lose a direct mutation that lands between prepare and
// commit — one more reason not to mix.
//
// Caller holds l.mu.
func (l *List) prepareUpsertLocked(entries []Entry) (*overlayBuild, error) {
	return l.prepareOverlayLocked(func(src map[string]Entry, tomb map[string]struct{}, base *segment) {
		for _, e := range entries {
			src[e.ID] = e // map assignment: later duplicates win
			if base != nil {
				if _, inBase := base.byID[e.ID]; inBase {
					tomb[e.ID] = struct{}{} // superseded: mask the base version
				}
			}
		}
	})
}

// Caller holds l.mu.
func (l *List) prepareDeleteLocked(id string) (*overlayBuild, error) {
	return l.prepareOverlayLocked(func(src map[string]Entry, tomb map[string]struct{}, base *segment) {
		delete(src, id)
		if base != nil {
			if _, inBase := base.byID[id]; inBase {
				tomb[id] = struct{}{}
			}
		}
	})
}

// Compact folds base+overlay−tombstones into a fresh base segment (empty
// overlay, empty tombstones). The live-entry SET is unchanged, but ngram
// scores may shift: IDF weights and the known-gram denominator are segment-
// local and get recomputed over the folded corpus, so ranking and threshold
// survival can differ from the pre-compact base+overlay split. The new base
// is built BEFORE any state changes; a build error leaves the list untouched.
func (l *List) Compact() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	live := l.liveEntriesLocked()
	base, err := l.buildSegment(live)
	if err != nil {
		return err
	}
	l.gen++
	// The fold invalidates any content-addressed base identity (the Store
	// path uses PrepareCompact/CommitCompact and never comes through here).
	// Drop to the gen format rather than serve a stale hash over new
	// content.
	l.baseID = ""
	l.snap.Store(&snapshot{
		base:       base,
		tombstones: map[string]struct{}{},
		version:    fmt.Sprintf("gen%d-base@%d", l.gen, len(live)),
	})
	l.overlaySrc = map[string]Entry{}
	return nil
}

// liveEntriesLocked returns base−tombstones plus overlay, ID-sorted, overlay
// winning on conflicts. Caller holds l.mu.
func (l *List) liveEntriesLocked() []Entry {
	s := l.snap.Load()
	out := []Entry{}
	if s.base != nil {
		for _, e := range s.base.entries {
			if _, dead := s.tombstones[e.ID]; dead {
				continue
			}
			if _, shadowed := l.overlaySrc[e.ID]; shadowed {
				continue
			}
			out = append(out, e)
		}
	}
	for _, e := range l.overlaySrc {
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

func cloneSet(s map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(s))
	for k := range s {
		out[k] = struct{}{}
	}
	return out
}

// ParentMatchScore is the score for exact-mode hits found via parent_domain
// fallback (the full analyzed query missed; a parent suffix is listed).
// Exact-level hits stay at 100 — callers can treat 100 as "block" and 90 as
// "review", and Candidate.Key names the listed domain either way.
const ParentMatchScore = 90

// exactLevels returns the probe chain for an analyzed exact-mode query:
// the string itself, then (with parent_domain fallback) each parent suffix
// while at least two labels remain — bare TLDs are never probed.
func exactLevels(aq string, parentFallback bool) []string {
	levels := []string{aq}
	if !parentFallback {
		return levels
	}
	rest := aq
	for {
		i := strings.IndexByte(rest, '.')
		if i < 0 {
			break
		}
		rest = rest[i+1:]
		if strings.IndexByte(rest, '.') < 0 {
			break // one label left: a bare TLD, stop
		}
		levels = append(levels, rest)
	}
	return levels
}

// Query looks q up against base and overlay, masks tombstones, ranks, cuts to
// top-K, and attributes the best-matching key per candidate.
func (l *List) Query(q string, opts QueryOpts) []Candidate {
	return l.QueryCtx(context.Background(), q, opts)
}

// QueryCtx is Query with cooperative cancellation: the ngram scan polls ctx
// periodically (see ngram.LookupCtx) and the whole query returns nil when
// canceled — ambiguous with "no match" by design; callers that care check
// ctx.Err(). The exact path takes no checks: its work is bounded by the
// probe-level count and the topK collection cap, microseconds at any scale.
func (l *List) QueryCtx(ctx context.Context, q string, opts QueryOpts) []Candidate {
	c, _ := l.QueryVersioned(ctx, q, opts)
	return c
}

// testHookQuerySnapshot runs after QueryVersioned has loaded its snapshot
// and before the query executes — the window in which a concurrent mutation
// must NOT be able to change which version the answer is attributed to.
// Tests set it; it is nil in every other build.
var testHookQuerySnapshot func()

// QueryVersioned is QueryCtx additionally returning the version stamp of
// the snapshot that produced the candidates. Candidates and version come
// from ONE atomic snapshot load: an answer's version is its evidence, and
// reading Version() separately after the query would let a concurrent
// mutation stamp one snapshot's candidates with another snapshot's
// identity. Callers recording versions (audit, response envelopes) must use
// this instead of a separate Version() call.
func (l *List) QueryVersioned(ctx context.Context, q string, opts QueryOpts) ([]Candidate, string) {
	p := l.PrepareQuery(q, opts)
	if testHookQuerySnapshot != nil {
		testHookQuerySnapshot()
	}
	return p.Execute(ctx)
}

// PreparedQuery pins one snapshot of one list together with the query, its
// effective options, and the admission cost computed FROM THAT snapshot.
// Admission control must charge and execute the SAME snapshot: pricing the
// current snapshot, waiting on the budget, and then loading a fresh one
// lets a mutation that landed while the request queued grow the executed
// work arbitrarily past what was charged — the same
// time-of-check/time-of-use shape QueryVersioned closes for versions,
// here between memory evidence and execution.
type PreparedQuery struct {
	l    *List
	s    *snapshot
	q    string
	opts QueryOpts
	cost int64
}

// PrepareQuery captures the snapshot a query will run against and its
// conservative scratch cost. The returned value holds a reference to the
// captured snapshot until executed and dropped — prepare, admit, execute,
// in that order, promptly.
func (l *List) PrepareQuery(q string, opts QueryOpts) *PreparedQuery {
	s := l.snap.Load()
	topK := l.cfg.Match.TopK
	if opts.TopK > 0 {
		topK = opts.TopK
	} else if opts.TopK < 0 {
		topK = 0 // sentinel: explicit unlimited (see QueryOpts)
	}
	runes := utf8.RuneCountInString(q)
	if runes < scratchDefaultQueryRunes {
		runes = scratchDefaultQueryRunes
	}
	return &PreparedQuery{l: l, s: s, q: q, opts: opts, cost: scratchBytesSnap(s, topK, runes, len(l.cfg.Match.Grams))}
}

// Cost is the conservative scratch-memory charge for executing THIS
// prepared snapshot (see ScratchBytesFor for the model).
func (p *PreparedQuery) Cost() int64 { return p.cost }

// Execute runs the prepared query against its captured snapshot and
// returns candidates plus that snapshot's version — the exact state the
// admission charge was computed from.
func (p *PreparedQuery) Execute(ctx context.Context) ([]Candidate, string) {
	return p.l.querySnap(ctx, p.s, p.q, p.opts), p.s.version
}

// querySnap runs a query against one already-loaded snapshot (see
// QueryVersioned for why the load happens exactly once).
func (l *List) querySnap(ctx context.Context, s *snapshot, q string, opts QueryOpts) []Candidate {
	if ctx.Err() != nil {
		return nil
	}
	aq := l.an.Normalize(q)
	if aq == "" {
		return nil
	}
	thr := l.cfg.Match.Threshold
	if opts.Threshold > 0 {
		thr = opts.Threshold
	} else if opts.Threshold < 0 {
		thr = 0 // sentinel: explicit no-floor (see QueryOpts)
	}
	topK := l.cfg.Match.TopK
	if opts.TopK > 0 {
		topK = opts.TopK
	} else if opts.TopK < 0 {
		topK = 0 // sentinel: explicit unlimited (see QueryOpts)
	}

	var out []Candidate
	var meta []candMeta // parallel to out: origin segment+ordinal for attribution
	add := func(seg *segment, masked map[string]struct{}, ord uint32, score float64) {
		e := seg.entries[ord]
		if masked != nil {
			if _, dead := masked[e.ID]; dead {
				return
			}
		}
		out = append(out, Candidate{EntryID: e.ID, Score: score, Payload: e.Payload})
		meta = append(meta, candMeta{seg: seg, ord: ord})
	}
	// collectNgram gathers one ngram segment's hits; exact mode never calls it
	// (the exact path is the per-level loop below).
	collectNgram := func(seg *segment, masked map[string]struct{}) {
		if seg == nil || seg.ng == nil {
			return
		}
		// Per-segment cut at topK+len(masked) is safe: masking only
		// removes hits, so the global post-mask top-K is contained in
		// the segment's top-(K+masked). Tiebreak nuance: this cut breaks
		// equal-score ties ord-asc while the final merge breaks them
		// EntryID-asc, so which equal-scored candidates survive the cut
		// is deterministic but not EntryID-ordered. Ord order itself
		// depends on provenance: a Replace base keeps caller order while
		// a compacted base is ID-sorted, so equal-score cut survivors
		// can differ before vs after Compact.
		segK := 0
		if topK > 0 {
			segK = topK + len(masked)
		}
		for _, h := range seg.ng.LookupCtx(ctx, aq, thr, segK) {
			add(seg, masked, h.Ord, h.Score)
		}
	}
	matchedKey := aq
	if l.cfg.Match.Mode == "exact" {
		// Probe base + overlay together per level; descend to the next parent
		// suffix only when a level has NO post-tombstone survivors. `add`
		// applies masking before appending, so `len(out) > 0` is the correct
		// post-mask check — a fully tombstoned level must not stop descent.
		for _, level := range exactLevels(aq, l.cfg.Match.Fallback == "parent_domain") {
			score := 100.0
			if level != aq {
				score = ParentMatchScore
			}
			if score < thr*100 {
				continue // below the query threshold: thr > 0.9 suppresses parent levels
			}
			// Collection is capped at topK post-mask survivors: every hit at
			// one level shares the same score, so any K of them are a valid
			// top-K — collecting a hot key's full run just to truncate would
			// allocate and sort the whole hit set. Which ties survive follows
			// collection order (base then overlay, ordinal-ascending) — the
			// same documented stance as the ngram per-segment cut.
			full := func() bool { return topK > 0 && len(out) >= topK }
			if s.base != nil && s.base.ex != nil && !full() {
				for _, ord := range s.base.ex.Lookup(level) {
					add(s.base, s.tombstones, ord, score)
					if full() {
						break
					}
				}
			}
			if s.overlay != nil && s.overlay.ex != nil && !full() {
				for _, ord := range s.overlay.ex.Lookup(level) {
					add(s.overlay, nil, ord, score)
					if full() {
						break
					}
				}
			}
			if len(out) > 0 {
				matchedKey = level
				break // first level with post-mask hits wins
			}
		}
	} else {
		collectNgram(s.base, s.tombstones)
		collectNgram(s.overlay, nil)
	}
	if len(out) == 0 {
		return nil
	}
	// A scan canceled AFTER an earlier segment already contributed would
	// otherwise return those partial results as if complete (LookupCtx
	// returns nil for the canceled segment, indistinguishable from "that
	// segment had no hits"). Honor the documented contract — the whole
	// query returns nil once canceled.
	if ctx.Err() != nil {
		return nil
	}

	sortCandidates(out, meta)
	if topK > 0 && len(out) > topK {
		out = out[:topK]
		meta = meta[:topK]
	}
	l.attributeKeys(s, aq, matchedKey, out, meta)
	return out
}

// Version returns the snapshot version stamp. Store-managed lists carry the
// restart-stable content-addressed form
// "<baseID>@<baseEntries>+j<jbytes>[.<jhash>]+c<cfg>" — baseID = sha256 of
// base.jsonl (or "empty"); jbytes = journal byte position, replay depth;
// jhash = sha256 of the journal's exact byte content, omitted for an empty
// journal; cfg = sha256 of the RESOLVED list configuration (see
// resolvedConfigDigest), because the same data bytes queried under a
// different mode, analyzer, or default answer differently. The same disk
// state always yields the same version, and equal versions identify equal
// data under an equal configuration. Library-managed lists (no Store) keep
// the process-local "gen…" form — unique within a process, NOT
// restart-stable.
func (l *List) Version() string { return l.snap.Load().version }

// scratchPerHitBytes models ONE collected hit across every layer admission
// can see: the ngram Hit (16 B), the list-layer Candidate (~64 B of ID,
// score, key and payload headers) with its attribution meta, the server's
// response copy, and slice-growth overhead. Deliberately generous: the
// bounded hit set is at most a few thousand entries, so overcharging per
// hit costs the budget little while keeping the model safely above what
// the query really allocates.
const scratchPerHitBytes = 192

// ScratchBytesFor estimates the peak per-query memory a lookup on this list
// holds in flight when it runs with the given effective topK, per segment
// (base + overlay). It includes query buffers sized for at least a 512-rune
// query; PrepareQuery charges longer library-level queries from their actual
// rune count:
//
//   - 4 B × numOrds — the dense IDF accumulator (counts);
//   - 4 B × numOrds — the touched-ordinal list: a flood query (hot gram,
//     no-floor scan) can touch every ordinal, and the pool retains the
//     grown slice after the query;
//   - scratchPerHitBytes × the segment's hit-collection bound — topK plus
//     the tombstone mask for the base segment (the engine collects up to
//     topK+masked before masking), clamped to numOrds. topK <= 0 means
//     UNLIMITED collection: every ordinal can become a hit and is charged
//     as one.
//
// Exact-mode lists pay the same conservative per-hit materialization term,
// without the ngram accumulator/query buffers. Admission control sizes a
// concurrency budget against this; an undercharge here turns concurrency
// into an OOM, so every term errs upward. The cost model is MEMORY, but the
// same bound also caps how many maximal-CPU no-floor scans run at once.
func (l *List) ScratchBytesFor(topK int) int64 {
	return scratchBytesSnap(l.snap.Load(), topK, scratchDefaultQueryRunes, len(l.cfg.Match.Grams))
}

const (
	// The HTTP boundary accepts at most 512 runes. Charging at least that
	// shape keeps ScratchBytesFor useful for operators sizing a budget while
	// PrepareQuery still scales for direct-library callers with longer input.
	scratchDefaultQueryRunes = 512
	// Per ngram index: roaring iterator batch plus offsets, dedup-map storage,
	// gramInfo records, sorting and allocator slack. One potential gram per
	// rune per configured size is the conservative upper shape.
	scratchQueryFixedBytes   = 16 << 10
	scratchQueryPerGramBytes = 64
	scratchQueryPerRuneBytes = 8
)

// scratchBytesSnap is the model over one already-loaded snapshot — the
// form PrepareQuery uses, so the cost and the executed snapshot can never
// belong to different states.
func scratchBytesSnap(s *snapshot, topK, queryRunes, gramSizes int) int64 {
	masked := int64(len(s.tombstones))
	var b int64
	charge := func(sg *segment, masked int64) {
		if sg == nil {
			return
		}
		if sg.ex != nil {
			hits := int64(sg.ex.NumOrds())
			if topK > 0 && int64(topK) < hits {
				hits = int64(topK)
			}
			b += scratchPerHitBytes * hits
			return
		}
		if sg.ng == nil {
			return
		}
		ords := int64(sg.ng.NumOrds())
		b += 8 * ords // counts + touched, both preallocated at numOrds
		b += scratchQueryFixedBytes + int64(queryRunes)*scratchQueryPerRuneBytes
		b += int64(queryRunes) * int64(gramSizes) * scratchQueryPerGramBytes
		hits := ords
		if topK > 0 {
			if hb := int64(topK) + masked; hb < hits {
				hits = hb
			}
		}
		b += scratchPerHitBytes * hits
	}
	charge(s.base, masked)
	charge(s.overlay, 0)
	return b
}

// ScratchBytes is ScratchBytesFor at the unlimited-collection worst case —
// for callers sizing a budget without a concrete query shape. Callers that
// know the effective topK (the server does) should charge ScratchBytesFor
// instead.
func (l *List) ScratchBytes() int64 { return l.ScratchBytesFor(0) }

// ListStatus is one coherent view of a List snapshot for status and audit
// surfaces. Reading the individual legacy accessors in sequence can mix two
// mutations; this value is derived from one atomic snapshot load.
type ListStatus struct {
	Entries          int
	Overlay          int
	Tombstones       int
	Version          string
	DroppedKeys      int
	KeylessEntries   int
	UnindexedEntries int
}

// Status returns counters and version from one immutable snapshot.
func (l *List) Status() ListStatus {
	s := l.snap.Load()
	var out ListStatus
	out.Version = s.version
	if s.base != nil {
		out.Entries = len(s.base.entries)
		for id := range s.tombstones {
			if _, ok := s.base.byID[id]; ok {
				out.Tombstones++
			}
		}
	}
	out.Entries -= out.Tombstones
	if s.overlay != nil {
		out.Overlay = len(s.overlay.entries)
		out.Entries += out.Overlay
	}
	for _, seg := range []*segment{s.base, s.overlay} {
		if seg != nil {
			out.DroppedKeys += seg.droppedKeys
			out.KeylessEntries += seg.keylessEntries
			out.UnindexedEntries += seg.unindexedEntries
		}
	}
	return out
}

// KeyCount returns the live raw-key count: base keys minus tombstoned
// entries' keys plus overlay keys. Raw (pre-analysis) keys — the quota
// meters what the caller stores, not what survives analysis. O(tombstones).
func (l *List) KeyCount() int64 {
	s := l.snap.Load()
	var n int64
	if s.base != nil {
		n += int64(s.base.totalKeys)
		for id := range s.tombstones {
			if ord, ok := s.base.byID[id]; ok {
				n -= int64(len(s.base.entries[ord].Keys))
			}
		}
	}
	if s.overlay != nil {
		n += int64(s.overlay.totalKeys)
	}
	return n
}

// BuildStats returns the current snapshot's build-time loss counters, summed
// over base and overlay: keys the analyzer collapsed to "" (dropped — never
// indexed), and entries whose keys ALL collapsed (they occupy an ordinal and
// count in Stats() entries while being unfindable). For a caller loading a
// real-world list, a large droppedKeys is the difference between "loaded
// fine" and "half my list silently vanished".
func (l *List) BuildStats() (droppedKeys, keylessEntries int) {
	s := l.Status()
	return s.DroppedKeys, s.KeylessEntries
}

// IndexBuildInfo is what a prebuilt index cannot state about itself: how
// many base entries it was built from, and that build's loss counters.
//
// The entry count is the load-bearing part. An index reports its highest
// ordinal, which drops both when the artifact is STALE (built from fewer
// entries than base.jsonl now holds) and when the final entries analyzed to
// nothing — indistinguishable causes with different repairs. Carrying the
// count separates them.
type IndexBuildInfo struct {
	Entries        int
	DroppedKeys    int
	KeylessEntries int
}

// applyBuildInfo sets a prebuilt segment's loss counters. With no info the
// segment claims nothing: reporting zero for something unknown is wrong, but
// inferring it from the index is wrong in a way that names the wrong repair.
func (seg *segment) applyBuildInfo(bi *IndexBuildInfo, n int, ords uint32) error {
	if bi == nil {
		return nil
	}
	// The library boundary gets the same refusal the artifact loader gives
	// a malformed record: impossible values would surface as negative
	// metrics and a fabricated unindexed count.
	if bi.Entries < 0 || bi.DroppedKeys < 0 || bi.KeylessEntries < 0 ||
		bi.KeylessEntries > bi.Entries || bi.Entries > n {
		return fmt.Errorf("build info %+v is impossible for %d entries", *bi, n)
	}
	// And internally consistent with the index itself: postings cannot
	// name ordinals past the entry count the record claims produced them.
	// Accepting the contradiction reports reachable entries as unindexed.
	if int(ords) > bi.Entries {
		return fmt.Errorf("index has %d ordinals but build info claims %d entries", ords, bi.Entries)
	}
	seg.droppedKeys = bi.DroppedKeys
	seg.keylessEntries = bi.KeylessEntries
	if u := n - bi.Entries; u > 0 {
		seg.unindexedEntries = u
	}
	return nil
}

// UnindexedEntries returns how many entries the current snapshot holds that
// its index cannot reach, summed over base and overlay. Non-zero means a
// base.idx covering fewer entries than base.jsonl was installed —
// ReplaceWithIndex tolerates that deliberately, so a stale-index crash
// window stays recoverable instead of fatal, but the entries past the
// index's last ordinal will never match anything until the list is rebuilt
// or compacted. It is separate from BuildStats because the repair differs:
// dropped/keyless keys are an analyzer question, this is a stale artifact.
//
// It is not part of BuildStats' return only because adding a third value
// there would break every existing caller.
func (l *List) UnindexedEntries() int {
	return l.Status().UnindexedEntries
}

// Stats returns entry count (live), overlay size, and tombstone count. Only
// tombstones actually present in base are counted — defensive; the snapshot
// invariants make any others impossible.
func (l *List) Stats() (entries, overlay, tombstones int) {
	s := l.Status()
	return s.Entries, s.Overlay, s.Tombstones
}
