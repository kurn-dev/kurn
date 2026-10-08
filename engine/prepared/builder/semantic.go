package builder

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	"github.com/RoaringBitmap/roaring"
	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/exact"
	"github.com/kurn-dev/kurn/engine/ngram"
	"github.com/kurn-dev/kurn/engine/prepared"
)

// Semantic verification rebuilds an artifact from its own records and
// compares. After full verification (prepared.LevelFull: structure, header
// and section digests, the logical base ID recomputed from the records, the
// container digest) it establishes three more things with the engine's own
// code:
//
//   - configuration identity: the config and analyzer digests the engine
//     computes for the stored configuration equal the declared ones;
//   - the index: the index section is byte for byte the one the engine's
//     index builder produces from the records in base order;
//   - the derivable loss counters (dropped_keys, keyless_entries) equal
//     what that build counts.
//
// It shows that the index, configuration identity and derivable counters
// are what the records produce. It does NOT authenticate who produced the
// artifact — anyone can build a self-consistent artifact — and it says
// nothing about source material absent from the artifact: the source
// reference, created_at, input_sha256 and the duplicate-collapse count
// describe input the records no longer contain, so they stay declared.

// CounterCheck compares one derivable build-section counter.
type CounterCheck struct {
	Name       string // the build-section field, e.g. "dropped_keys"
	Declared   int
	Recomputed int
	OK         bool
}

// SectionCheck is the byte-for-byte comparison of one section of the
// artifact with the same section of the artifact rebuilt from its records.
type SectionCheck struct {
	Kind string // "config", "build", "records", "ids", "ngram", "exact" or "exact_companion"
	OK   bool
	// Offset is the first differing byte, relative to the section start
	// (-1 when OK). When one section is a prefix of the other it is the
	// shorter length.
	Offset int64
	// FileOffset is Offset as an offset into the artifact file (-1 when OK).
	FileOffset int64
}

// DeclaredOnly holds build-section values that describe input the
// artifact does not contain, so no check can reproduce them from the
// artifact. They are reported, never verified.
type DeclaredOnly struct {
	Source      string
	CreatedAt   string
	InputSHA256 string
	Collapsed   int // byte-identical duplicates collapsed before the build
	// UnknownSections lists optional section kinds this build does not
	// know; they are covered by the digests but not interpreted.
	UnknownSections []uint32
}

// SemanticReport is what VerifySemanticBytes established. It holds copies
// only, never references into the verified buffer.
type SemanticReport struct {
	// Verified is what full verification established (level "full").
	Verified prepared.Verified

	// Configuration identity: declared digests are Verified.ConfigDigest
	// and Verified.AnalyzerDigest.
	RecomputedConfigDigest   string
	RecomputedAnalyzerDigest string
	ConfigDigestOK           bool
	AnalyzerDigestOK         bool

	// The index section rebuilt from the records.
	IndexKind string // "ngram" or "exact"
	IndexOK   bool
	// IndexMismatchOffset is the first differing byte of the index
	// section, relative to its start (-1 when IndexOK).
	IndexMismatchOffset int64
	// IndexMismatch describes the first logical difference when one is
	// cheaply derivable (a gram or key present on one side only, or a
	// posting difference), else the byte-level fact.
	IndexMismatch string

	// The exact companion section (an ngram list configured with
	// exact_companion) rebuilt from the records: CompanionOK is true when
	// it matches, and when the artifact has none. CompanionMismatch
	// describes the first logical difference, as IndexMismatch does.
	CompanionOK       bool
	CompanionMismatch string

	// Derivable loss counters: dropped_keys and keyless_entries.
	CountersOK bool
	Counters   []CounterCheck

	// Sections is the byte-for-byte comparison of every section with the
	// rebuilt artifact's, in kind order (config, build, records, ids,
	// index, then the exact companion when present). The index entry
	// repeats IndexOK and the companion entry CompanionOK.
	Sections []SectionCheck

	// Declared lists what the artifact cannot prove about itself.
	Declared DeclaredOnly

	// Failures names every failed check (empty when OK).
	Failures []string
}

// OK reports whether every semantic check passed.
func (r *SemanticReport) OK() bool {
	if r == nil || !r.ConfigDigestOK || !r.AnalyzerDigestOK || !r.IndexOK || !r.CompanionOK || !r.CountersOK {
		return false
	}
	for _, s := range r.Sections {
		if !s.OK {
			return false
		}
	}
	return len(r.Failures) == 0
}

// VerifySemantic reads the artifact at path once (prepared.ReadFile) and
// verifies that buffer with VerifySemanticBytes.
func VerifySemantic(path string) (*SemanticReport, error) {
	buf, err := prepared.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return VerifySemanticBytes(buf)
}

// VerifySemanticBytes runs prepared.VerifyBytes(buf, prepared.LevelFull)
// itself on buf, then the semantic checks over that same buffer: it
// recomputes the config and analyzer digests with the engine, rebuilds the
// index from the records in base order with the engine's own builder and
// compares the encoded section byte for byte, and recomputes the derivable
// loss counters. See the package's semantic-verification comment for what
// this does and does not establish (in short: the index, configuration
// identity and derivable counters are what the records produce; the
// producer is not authenticated).
//
// Callers must not mutate buf during the call. The returned report holds
// no references into buf.
//
// Errors from full verification are returned unchanged (errors.Is sees
// prepared.ErrMalformed, ErrIncompatible, ErrUnaligned or ErrIntegrity),
// with a nil report. A configuration or record set the engine refuses to
// resolve or rebuild wraps prepared.ErrIncompatible, with a nil report. A
// semantic mismatch wraps prepared.ErrIntegrity, names every failed check,
// and comes with the report.
func VerifySemanticBytes(buf []byte) (*SemanticReport, error) {
	v, ver, err := prepared.VerifyBytes(buf, prepared.LevelFull)
	if err != nil {
		return nil, err
	}
	r := &SemanticReport{Verified: cloneVerified(ver)}
	b := v.Build()
	r.Declared = DeclaredOnly{
		Source: strings.Clone(b.Source), CreatedAt: strings.Clone(b.CreatedAt),
		InputSHA256: strings.Clone(b.InputSHA256), Collapsed: b.Collapsed,
	}
	origSecs := sectionSpans(buf)
	for _, s := range origSecs {
		if s.kind < prepared.KindConfig || s.kind > prepared.KindExactCompanion {
			r.Declared.UnknownSections = append(r.Declared.UnknownSections, s.kind)
		}
	}

	// 1. Configuration identity, recomputed by the engine.
	rc, err := resolveConfig(v)
	if err != nil {
		return nil, fmt.Errorf("%w: semantic verification: the engine cannot resolve the artifact's list configuration: %v", prepared.ErrIncompatible, err)
	}
	c := v.Config()
	r.RecomputedConfigDigest, r.RecomputedAnalyzerDigest = rc.configDigest, rc.analyzerDigest
	r.ConfigDigestOK = rc.configDigest == c.ConfigDigest
	r.AnalyzerDigestOK = rc.analyzerDigest == c.AnalyzerDigest
	if !r.ConfigDigestOK {
		r.fail("config digest: declared %s, the configuration gives %s", c.ConfigDigest, rc.configDigest)
	}
	if !r.AnalyzerDigestOK {
		r.fail("analyzer digest: declared %s, the configuration gives %s", c.AnalyzerDigest, rc.analyzerDigest)
	}

	// 2. Rebuild from the records in base order, exactly as Encode builds a
	// fresh artifact: the same Prepare (engine NewList + Replace, the
	// engine's own index builder and loss counters) and the same encoder.
	// The declared-only provenance is carried over verbatim so that the
	// build section differs only where a derivable value does.
	entries := make([]engine.Entry, v.Len())
	for i := range entries {
		entries[i] = engine.Entry{ID: v.ID(i), Keys: v.Keys(i), Payload: v.Payload(i)}
	}
	rebuilt, err := Prepare(c.Name, rc.cfg, entries, Options{
		Source: b.Source, CreatedAt: b.CreatedAt, InputSHA256: b.InputSHA256, Collapsed: b.Collapsed,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: semantic verification: the engine cannot rebuild the artifact from its records: %v", prepared.ErrIncompatible, err)
	}
	// The rebuild keeps the declared digests, so that the config section
	// comparison below isolates everything but them (they were compared
	// above, and the rebuilt ones are exactly the recomputed ones).
	rebuilt.ConfigDigest, rebuilt.AnalyzerDigest = c.ConfigDigest, c.AnalyzerDigest
	rbuf, _, err := prepared.Encode(rebuilt)
	if err != nil {
		return nil, fmt.Errorf("%w: semantic verification: re-encoding the records failed: %v", prepared.ErrIncompatible, err)
	}
	newSecs := sectionSpans(rbuf)
	body := func(buf []byte, secs []span, kind uint32) ([]byte, uint64, bool) {
		for _, s := range secs {
			if s.kind == kind {
				return buf[s.off : s.off+s.length], s.off, true
			}
		}
		return nil, 0, false
	}

	// 3. Derivable loss counters, as the rebuild counted them.
	r.Counters = []CounterCheck{
		{Name: "dropped_keys", Declared: b.DroppedKeys, Recomputed: rebuilt.DroppedKeys},
		{Name: "keyless_entries", Declared: b.KeylessEntries, Recomputed: rebuilt.KeylessEntries},
	}
	r.CountersOK = true
	for i := range r.Counters {
		k := &r.Counters[i]
		k.OK = k.Declared == k.Recomputed
		if !k.OK {
			r.CountersOK = false
			r.fail("%s: declared %d, the records give %d", k.Name, k.Declared, k.Recomputed)
		}
	}

	// Byte-for-byte section comparison.
	indexKind := prepared.KindNgram
	r.IndexKind = "ngram"
	if c.Mode == "exact" {
		indexKind, r.IndexKind = prepared.KindExact, "exact"
	}
	kinds := []uint32{prepared.KindConfig, prepared.KindBuild, prepared.KindRecords, prepared.KindIDs, indexKind}
	if c.ExactCompanion {
		kinds = append(kinds, prepared.KindExactCompanion)
	}
	r.IndexOK, r.CompanionOK = true, true
	for _, kind := range kinds {
		ob, oOff, _ := body(buf, origSecs, kind)
		nb, _, ok := body(rbuf, newSecs, kind)
		sc := SectionCheck{Kind: kindName(kind), OK: true, Offset: -1, FileOffset: -1}
		if !ok || !bytes.Equal(ob, nb) {
			sc.OK = false
			sc.Offset = int64(firstDiff(ob, nb))
			sc.FileOffset = int64(oOff) + sc.Offset
		}
		r.Sections = append(r.Sections, sc)
		if sc.OK {
			continue
		}
		switch kind {
		case indexKind:
			r.IndexOK = false
			r.IndexMismatchOffset = sc.Offset
			if indexKind == prepared.KindNgram {
				r.IndexMismatch = describeNgramDiff(v, rebuilt.Ngram)
			} else {
				r.IndexMismatch = describeExactDiff(v, rebuilt.Exact)
			}
			if r.IndexMismatch == "" {
				r.IndexMismatch = "the postings agree as sets but are encoded differently"
			}
			r.fail("index (%s section) differs from the index the records produce at section offset %d: %s", r.IndexKind, sc.Offset, r.IndexMismatch)
		case prepared.KindExactCompanion:
			r.CompanionOK = false
			r.CompanionMismatch = describeExactCompanionDiff(v, rebuilt.ExactCompanion)
			if r.CompanionMismatch == "" {
				r.CompanionMismatch = "the postings agree as sets but are encoded differently"
			}
			r.fail("exact companion section differs from the companion the records produce at section offset %d: %s", sc.Offset, r.CompanionMismatch)
		case prepared.KindBuild:
			if r.CountersOK { // otherwise already named by the counters
				r.fail("build section does not re-encode identically (section offset %d)", sc.Offset)
			}
		default:
			// The config section can differ only through the digests
			// (the rebuild carries the declared ones), and the records
			// and ids sections not at all for an artifact that passed full
			// verification; check them anyway.
			r.fail("%s section does not re-encode identically (section offset %d)", sc.Kind, sc.Offset)
		}
	}
	if r.IndexOK {
		r.IndexMismatchOffset = -1
	}
	if len(r.Failures) > 0 {
		return r, fmt.Errorf("%w: semantic verification failed: %s", prepared.ErrIntegrity, strings.Join(r.Failures, "; "))
	}
	return r, nil
}

func (r *SemanticReport) fail(format string, args ...any) {
	r.Failures = append(r.Failures, fmt.Sprintf(format, args...))
}

// cloneVerified copies v so that no string in the report can alias the
// verified buffer.
func cloneVerified(v *prepared.Verified) prepared.Verified {
	out := *v
	for _, s := range []*string{&out.Name, &out.Mode, &out.BaseID, &out.ContainerSHA256, &out.DeclaredBaseID, &out.ConfigDigest, &out.AnalyzerDigest} {
		*s = strings.Clone(*s)
	}
	return out
}

// span is one section table entry.
type span struct {
	kind        uint32
	off, length uint64
}

// sectionSpans reads the section table of an artifact that already passed
// prepared's structural checks (the layout is in package prepared's doc):
// a 24-byte header whose u32 at offset 16 is the section count, then
// 64-byte entries of kind u32, flags u32, offset u64, length u64.
func sectionSpans(buf []byte) []span {
	n := int(binary.LittleEndian.Uint32(buf[16:]))
	out := make([]span, n)
	for i := range out {
		e := buf[24+64*i:]
		out[i] = span{kind: binary.LittleEndian.Uint32(e[0:]), off: binary.LittleEndian.Uint64(e[8:]), length: binary.LittleEndian.Uint64(e[16:])}
	}
	return out
}

func kindName(k uint32) string {
	switch k {
	case prepared.KindConfig:
		return "config"
	case prepared.KindBuild:
		return "build"
	case prepared.KindRecords:
		return "records"
	case prepared.KindIDs:
		return "ids"
	case prepared.KindNgram:
		return "ngram"
	case prepared.KindExact:
		return "exact"
	case prepared.KindExactCompanion:
		return "exact_companion"
	}
	return fmt.Sprintf("kind %d", k)
}

// firstDiff returns the first index at which a and b differ (the shorter
// length when one is a prefix of the other). a and b must differ.
func firstDiff(a, b []byte) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// recordName names ordinal o of the artifact for a mismatch description.
func recordName(v *prepared.View, o uint32) string {
	if int64(o) < int64(v.Len()) {
		return fmt.Sprintf("ordinal %d (record %q)", o, v.ID(int(o)))
	}
	return fmt.Sprintf("ordinal %d", o)
}

// describeNgramDiff names the first logical difference between the
// artifact's ngram index and the rebuilt one, or "" if they agree as sets.
func describeNgramDiff(v *prepared.View, rebuilt *ngram.Index) string {
	post := rebuilt.Postings()
	grams := make([]string, 0, len(post))
	for g := range post {
		grams = append(grams, g)
	}
	sort.Strings(grams)
	i, j := 0, 0
	for i < v.GramCount() || j < len(grams) {
		switch {
		case j == len(grams) || (i < v.GramCount() && v.Gram(i) < grams[j]):
			return fmt.Sprintf("gram %q is in the artifact's index, but the records do not produce it", v.Gram(i))
		case i == v.GramCount() || grams[j] < v.Gram(i):
			return fmt.Sprintf("gram %q is produced by the records, but missing from the artifact's index", grams[j])
		}
		g := v.Gram(i)
		abm, err := v.Postings(i)
		if err != nil {
			return fmt.Sprintf("gram %q: %v", g, err)
		}
		if rbm := post[grams[j]]; !abm.Equals(rbm) {
			d := roaring.Xor(abm, rbm).Minimum()
			if abm.Contains(d) {
				return fmt.Sprintf("gram %q: the artifact posts %s, the records do not", g, recordName(v, d))
			}
			return fmt.Sprintf("gram %q: the records post %s, the artifact does not", g, recordName(v, d))
		}
		i++
		j++
	}
	if a, b := v.NumOrds(), rebuilt.NumOrds(); a != b {
		return fmt.Sprintf("the artifact's index covers %d ordinals, the records' %d", a, b)
	}
	return ""
}

// describeExactDiff names the first logical difference between the
// artifact's exact index and the rebuilt one, or "" if they agree.
func describeExactDiff(v *prepared.View, rebuilt *exact.Index) string {
	arena, keyLens, runLens, postings, err := v.ExactParts()
	if err != nil {
		return err.Error()
	}
	return describeExactPartsDiff(v, arena, keyLens, runLens, postings, rebuilt)
}

// describeExactCompanionDiff is describeExactDiff for the exact companion
// section.
func describeExactCompanionDiff(v *prepared.View, rebuilt *exact.Index) string {
	arena, keyLens, runLens, postings, err := v.ExactCompanionParts()
	if err != nil {
		return err.Error()
	}
	if rebuilt == nil {
		return "the records produce no exact companion index"
	}
	return describeExactPartsDiff(v, arena, keyLens, runLens, postings, rebuilt)
}

// describeExactPartsDiff compares one exact-layout section, given as its
// decoded parts, with a rebuilt index.
func describeExactPartsDiff(v *prepared.View, arena []byte, keyLens, runLens []int, postings []uint32, rebuilt *exact.Index) string {
	keys := rebuilt.SortedKeys()
	var aOff, pOff int
	i, j := 0, 0
	for i < len(keyLens) || j < len(keys) {
		var ak string
		if i < len(keyLens) {
			ak = string(arena[aOff : aOff+keyLens[i]])
		}
		switch {
		case j == len(keys) || (i < len(keyLens) && ak < keys[j]):
			return fmt.Sprintf("key %q is in the artifact's index, but the records do not produce it", ak)
		case i == len(keyLens) || keys[j] < ak:
			return fmt.Sprintf("key %q is produced by the records, but missing from the artifact's index", keys[j])
		}
		run := postings[pOff : pOff+runLens[i]]
		if want := rebuilt.Lookup(keys[j]); !equalU32(run, want) {
			d, inArtifact := firstSymDiff(run, want)
			if inArtifact {
				return fmt.Sprintf("key %q: the artifact posts %s, the records do not", ak, recordName(v, d))
			}
			return fmt.Sprintf("key %q: the records post %s, the artifact does not", ak, recordName(v, d))
		}
		aOff += keyLens[i]
		pOff += runLens[i]
		i++
		j++
	}
	return ""
}

func equalU32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// firstSymDiff returns the smallest value in exactly one of two ascending
// runs, and whether it is in a. The runs must differ.
func firstSymDiff(a, b []uint32) (uint32, bool) {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			i++
			j++
		case a[i] < b[j]:
			return a[i], true
		default:
			return b[j], false
		}
	}
	if i < len(a) {
		return a[i], true
	}
	return b[j], false
}
