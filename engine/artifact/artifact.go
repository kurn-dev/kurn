// Package artifact persists base indexes — ngram (KURNIDX1: magic header,
// JSON meta, then length-prefixed gram/bitmap pairs) and exact (KURNEXA1, see
// exact.go). Saves are atomic (temp+rename, fsynced) and deterministic (keys
// written in sorted order, so identical indexes produce byte-identical
// files). Corruption rejection is structural only (no checksum): a bit-flip
// inside a payload can deserialize as different valid data — acceptable for
// the experiment.
package artifact

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/RoaringBitmap/roaring"
	"github.com/kurn-dev/kurn/engine/ngram"
)

const magic = "KURNIDX1"

// maxLen bounds header and gram-key lengths on Load so a corrupt uvarint
// cannot trigger a giant allocation.
const maxLen = 1 << 20

// minPairBytes is the smallest possible serialized gram/bitmap pair (1-byte
// uvarint length + 1-byte gram + ~8-byte roaring bitmap); a header claiming
// more pairs than fileSize/minPairBytes is hostile or corrupt.
const minPairBytes = 10

// maxMapHint caps the postings map preallocation hint so a hostile gram_count
// cannot allocate gigabytes up front; legit larger loads just grow the map.
const maxMapHint = 1 << 20

type meta struct {
	Grams       []int  `json:"grams"`
	StripSpaces bool   `json:"strip_spaces"`
	NumOrds     uint32 `json:"num_ords"`
	GramCount   int    `json:"gram_count"`
	// Analyzer is the digest of the analyzer's canonical step spec the index
	// was built with ("" in artifacts written before this field existed —
	// treated as unknown by the install path, which rebuilds rather than
	// risk keys analyzed one way and queries another).
	Analyzer string `json:"analyzer,omitempty"`
	// Build is what the index cannot state about itself: how many base
	// entries it was built from, plus that build's loss counters. Without
	// the entry count a reader cannot tell a STALE artifact (built from
	// fewer entries than base.jsonl now holds) from a correct one whose
	// last entries analyzed to nothing — both simply lower NumOrds. Absent
	// in artifacts written before this field existed; nil then means
	// "unknown", and the install path rebuilds rather than guess, exactly
	// as it does for a pre-digest artifact.
	Build *BuildInfo `json:"build,omitempty"`
}

// BuildInfo travels with an index so its loss counters survive a reload,
// and — the load-bearing half — binds the index to the exact base content
// whose ordinal assignments it encodes.
type BuildInfo struct {
	// BaseID is the content-addressed identity of the base.jsonl the index
	// was built from (the version stamp's hash half). An index is a map
	// from grams to ORDINAL POSITIONS in that specific file: against any
	// other content of the same length every check can pass while the
	// postings point at different entities, so a query returns entry X
	// carrying entry Y's matching evidence. Counts cannot detect that;
	// only identity can.
	BaseID         string `json:"base_id"`
	Entries        int    `json:"entries"`
	DroppedKeys    int    `json:"dropped_keys"`
	KeylessEntries int    `json:"keyless_entries"`
}

// validate rejects records no build can produce. A malformed record is
// treated exactly like a corrupt artifact section: the load fails and the
// store rebuilds, rather than serving impossible metrics.
func (b *BuildInfo) validate() error {
	switch {
	case b == nil:
		return nil // absent: pre-record artifact, caller rebuilds
	case b.Entries < 0 || b.DroppedKeys < 0 || b.KeylessEntries < 0:
		return fmt.Errorf("artifact: build record with negative counts (%+v)", *b)
	case b.KeylessEntries > b.Entries:
		return fmt.Errorf("artifact: build record claims %d keyless of %d entries", b.KeylessEntries, b.Entries)
	}
	return nil
}

func Save(path string, idx *ngram.Index, analyzerDigest string, build BuildInfo) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".idx-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriterSize(tmp, 1<<20)

	if _, err := w.WriteString(magic); err != nil {
		return err
	}
	cfg := idx.Cfg()
	post := idx.Postings()
	hdr, _ := json.Marshal(meta{Grams: cfg.Grams, StripSpaces: cfg.StripSpaces, NumOrds: idx.NumOrds(), GramCount: len(post), Analyzer: analyzerDigest, Build: &build})
	if err := writeUvarint(w, uint64(len(hdr))); err != nil {
		return err
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	// Sorted keys: map iteration order is random, and byte-identical artifacts
	// for identical indexes are worth the O(n log n).
	keys := make([]string, 0, len(post))
	for g := range post {
		keys = append(keys, g)
	}
	sort.Strings(keys)
	for _, g := range keys {
		if err := writeUvarint(w, uint64(len(g))); err != nil {
			return err
		}
		if _, err := w.WriteString(g); err != nil {
			return err
		}
		if _, err := post[g].WriteTo(w); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil { // durable before the rename makes it visible
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func Load(path string) (*ngram.Index, string, *BuildInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, "", nil, err
	}
	r := bufio.NewReaderSize(f, 1<<20)

	got := make([]byte, len(magic))
	if _, err := io.ReadFull(r, got); err != nil || string(got) != magic {
		return nil, "", nil, fmt.Errorf("artifact: bad magic in %s", path)
	}
	hlen, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, "", nil, fmt.Errorf("artifact: header: %w", err)
	}
	if hlen == 0 || hlen > maxLen {
		return nil, "", nil, fmt.Errorf("artifact: header length %d out of range", hlen)
	}
	hdr := make([]byte, hlen)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, "", nil, fmt.Errorf("artifact: header: %w", err)
	}
	var m meta
	if err := json.Unmarshal(hdr, &m); err != nil {
		return nil, "", nil, fmt.Errorf("artifact: header: %w", err)
	}
	if len(m.Grams) == 0 {
		return nil, "", nil, fmt.Errorf("artifact: header: missing grams")
	}
	if m.GramCount < 0 {
		return nil, "", nil, fmt.Errorf("artifact: header: negative gram_count %d", m.GramCount)
	}
	if int64(m.GramCount) > fi.Size()/minPairBytes {
		return nil, "", nil, fmt.Errorf("artifact: header: gram_count %d implausible for %d-byte file", m.GramCount, fi.Size())
	}
	hint := m.GramCount
	if hint > maxMapHint {
		hint = maxMapHint
	}
	postings := make(map[string]*roaring.Bitmap, hint)
	for i := 0; i < m.GramCount; i++ {
		glen, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, "", nil, fmt.Errorf("artifact: gram %d: %w", i, err)
		}
		if glen == 0 || glen > maxLen {
			return nil, "", nil, fmt.Errorf("artifact: gram %d: length %d out of range", i, glen)
		}
		g := make([]byte, glen)
		if _, err := io.ReadFull(r, g); err != nil {
			return nil, "", nil, fmt.Errorf("artifact: gram %d: %w", i, err)
		}
		bm := roaring.New()
		if _, err := bm.ReadFrom(r); err != nil {
			return nil, "", nil, fmt.Errorf("artifact: bitmap %d: %w", i, err)
		}
		// Map assignment would silently keep only the last bitmap for a
		// repeated gram — a silently-wrong index. Save never writes
		// duplicates (sorted unique keys), so this is corruption; reject like
		// exact.Restore rejects duplicate keys.
		if _, dup := postings[string(g)]; dup {
			return nil, "", nil, fmt.Errorf("artifact: gram %d: duplicate gram %q", i, g)
		}
		postings[string(g)] = bm
	}
	if _, err := r.ReadByte(); err != io.EOF {
		return nil, "", nil, fmt.Errorf("artifact: trailing data after %d grams in %s", m.GramCount, path)
	}
	idx, err := ngram.Restore(ngram.Config{Grams: m.Grams, StripSpaces: m.StripSpaces}, postings, m.NumOrds)
	if err != nil {
		return nil, "", nil, fmt.Errorf("artifact: %s: %w", path, err)
	}
	if err := m.Build.validate(); err != nil {
		return nil, "", nil, err
	}
	// An index holding ordinals cannot have been built from zero entries,
	// so a zero record is a caller that passed no real info (or a
	// hand-made file), not a genuinely empty list. Report it as absent
	// rather than let it read as "every entry is unindexed". NumOrds
	// bounded by the claimed entry count for the same reason.
	build := m.Build
	if build != nil && build.Entries == 0 && m.NumOrds > 0 {
		build = nil
	}
	if build != nil && int(m.NumOrds) > build.Entries {
		return nil, "", nil, fmt.Errorf("artifact: %d ordinals from a claimed %d entries", m.NumOrds, build.Entries)
	}
	return idx, m.Analyzer, build, nil
}

func writeUvarint(w *bufio.Writer, v uint64) error {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], v)
	_, err := w.Write(buf[:n])
	return err
}
