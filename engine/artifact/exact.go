package artifact

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kurn-dev/kurn/engine/exact"
)

// exactMagic heads the exact-index artifact (KURNEXA1), the exact-mode
// sibling of KURNIDX1: magic, uvarint-length-prefixed JSON meta, then three
// sections — arena (every key's bytes back to back), per-key uvarint
// (keylen, runlen) pairs, and postings as little-endian uint32s. Two distinct
// layout properties: CONTIGUITY is load-bearing — offsets are never stored,
// LoadExact/Restore reconstruct them by accumulating the lens, so key i's
// bytes and run must sit immediately after key i-1's; SORTED key order is
// for determinism only — saving re-canonicalizes Finish's nondeterministic
// map layout so identical indexes produce byte-identical files (a load would
// succeed in any key order). Saves are atomic (temp+rename, fsynced).
// Corruption rejection is structural only, like KURNIDX1: section lengths,
// bounds, and trailing bytes are checked, but a bit-flip inside a posting
// can still decode as a different valid ordinal (no checksum) — out-of-range
// ordinals are caught by the install path's entry-count validation, and
// within-run ordinal monotonicity/cross-run duplicates go unvalidated too:
// wrong-but-safe results, never a panic (NumOrds bounds every ordinal, and
// ranking re-sorts candidates).
const exactMagic = "KURNEXA1"

// minExactKeyBytes is the smallest possible serialized footprint of one key
// (1 arena byte + 1-byte keylen uvarint + 1-byte runlen uvarint + one 4-byte
// posting); a header claiming more keys than fileSize/minExactKeyBytes is
// hostile or corrupt.
const minExactKeyBytes = 7

// maxSliceHint caps up-front slice preallocation from header-declared counts
// (same rationale as maxMapHint): legit larger loads just grow.
const maxSliceHint = 1 << 20

type exactMeta struct {
	KeyCount    int `json:"key_count"`
	PostingsLen int `json:"postings_len"`
	ArenaLen    int `json:"arena_len"`
	// Analyzer is the digest of the analyzer's canonical step spec the index
	// was built with ("" in artifacts written before this field existed —
	// treated as unknown by the install path, which rebuilds). Exact indexes
	// have no index-time knobs beyond the analyzer, so this digest IS the
	// whole config-identity check for KURNEXA1.
	Analyzer string `json:"analyzer,omitempty"`
}

// SaveExact atomically writes idx to path in KURNEXA1 format.
func SaveExact(path string, idx *exact.Index, analyzerDigest string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".idx-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriterSize(tmp, 1<<20)

	if _, err := w.WriteString(exactMagic); err != nil {
		return err
	}
	keys := idx.SortedKeys()
	arenaLen, postLen := 0, 0
	for _, k := range keys {
		arenaLen += len(k)
		postLen += len(idx.Lookup(k))
	}
	hdr, _ := json.Marshal(exactMeta{KeyCount: len(keys), PostingsLen: postLen, ArenaLen: arenaLen, Analyzer: analyzerDigest})
	if err := writeUvarint(w, uint64(len(hdr))); err != nil {
		return err
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	for _, k := range keys { // arena: key bytes back to back
		if _, err := w.WriteString(k); err != nil {
			return err
		}
	}
	for _, k := range keys { // per-key lens
		if err := writeUvarint(w, uint64(len(k))); err != nil {
			return err
		}
		if err := writeUvarint(w, uint64(len(idx.Lookup(k)))); err != nil {
			return err
		}
	}
	var le [4]byte
	for _, k := range keys { // postings, runs in sorted-key order
		for _, o := range idx.Lookup(k) {
			binary.LittleEndian.PutUint32(le[:], o)
			if _, err := w.Write(le[:]); err != nil {
				return err
			}
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

// LoadExact reads a KURNEXA1 file back into an exact.Index, also returning
// the analyzer digest recorded in the header ("" for pre-digest artifacts).
// Any structural problem — bad magic, implausible header, short section,
// trailing bytes, inconsistent lengths — is an error; the caller falls back
// to a rebuild.
func LoadExact(path string) (*exact.Index, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	r := bufio.NewReaderSize(f, 1<<20)

	got := make([]byte, len(exactMagic))
	if _, err := io.ReadFull(r, got); err != nil || string(got) != exactMagic {
		return nil, "", fmt.Errorf("artifact: bad exact magic in %s", path)
	}
	hlen, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, "", fmt.Errorf("artifact: exact header: %w", err)
	}
	if hlen == 0 || hlen > maxLen {
		return nil, "", fmt.Errorf("artifact: exact header length %d out of range", hlen)
	}
	hdr := make([]byte, hlen)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, "", fmt.Errorf("artifact: exact header: %w", err)
	}
	var m exactMeta
	if err := json.Unmarshal(hdr, &m); err != nil {
		return nil, "", fmt.Errorf("artifact: exact header: %w", err)
	}
	// Internal consistency: counts non-negative, every key needs >= 1 arena
	// byte and >= 1 posting.
	if m.KeyCount < 0 || m.PostingsLen < m.KeyCount || m.ArenaLen < m.KeyCount {
		return nil, "", fmt.Errorf("artifact: exact header: inconsistent counts key_count=%d postings_len=%d arena_len=%d", m.KeyCount, m.PostingsLen, m.ArenaLen)
	}
	// Plausibility against the file size, so a hostile header cannot trigger
	// giant allocations.
	if int64(m.KeyCount) > fi.Size()/minExactKeyBytes {
		return nil, "", fmt.Errorf("artifact: exact header: key_count %d implausible for %d-byte file", m.KeyCount, fi.Size())
	}
	if int64(m.ArenaLen) > fi.Size() {
		return nil, "", fmt.Errorf("artifact: exact header: arena_len %d implausible for %d-byte file", m.ArenaLen, fi.Size())
	}
	if int64(m.PostingsLen) > fi.Size()/4 {
		return nil, "", fmt.Errorf("artifact: exact header: postings_len %d implausible for %d-byte file", m.PostingsLen, fi.Size())
	}

	arena := make([]byte, m.ArenaLen)
	if _, err := io.ReadFull(r, arena); err != nil {
		return nil, "", fmt.Errorf("artifact: exact arena: %w", err)
	}
	keyLens := make([]int, 0, min(m.KeyCount, maxSliceHint))
	runLens := make([]int, 0, min(m.KeyCount, maxSliceHint))
	for i := 0; i < m.KeyCount; i++ {
		kl, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, "", fmt.Errorf("artifact: exact key %d length: %w", i, err)
		}
		rl, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, "", fmt.Errorf("artifact: exact key %d run length: %w", i, err)
		}
		if kl > uint64(m.ArenaLen) || rl > uint64(m.PostingsLen) {
			return nil, "", fmt.Errorf("artifact: exact key %d: lengths %d/%d exceed declared sections", i, kl, rl)
		}
		keyLens = append(keyLens, int(kl))
		runLens = append(runLens, int(rl))
	}
	// Bulk-read the postings section (its size is plausibility-bounded by the
	// file size above, so this cannot allocate more than the file holds).
	raw := make([]byte, 4*m.PostingsLen)
	if _, err := io.ReadFull(r, raw); err != nil {
		return nil, "", fmt.Errorf("artifact: exact postings: %w", err)
	}
	postings := make([]uint32, m.PostingsLen)
	for i := range postings {
		postings[i] = binary.LittleEndian.Uint32(raw[4*i:])
	}
	if _, err := r.ReadByte(); err != io.EOF {
		return nil, "", fmt.Errorf("artifact: trailing data after %d postings in %s", m.PostingsLen, path)
	}
	idx, err := exact.Restore(arena, keyLens, runLens, postings)
	if err != nil {
		return nil, "", fmt.Errorf("artifact: %s: %w", path, err)
	}
	return idx, m.Analyzer, nil
}
