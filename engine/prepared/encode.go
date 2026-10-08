package prepared

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"math"
	"sort"
	"unicode/utf8"

	"github.com/kurn-dev/kurn/engine/exact"
	"github.com/kurn-dev/kurn/engine/ngram"
)

// Record is one entry as the artifact stores it.
type Record struct {
	ID string
	// Keys are the raw (pre-analysis) keys. A nil slice is JSON null in the
	// canonical line, an empty one is [] — the two hash differently, so the
	// artifact preserves the distinction.
	Keys []string
	// Payload must already be canonical (see CanonicalPayload): the exact
	// bytes json.Marshal puts in the entry's base line. Empty means absent.
	Payload []byte
}

// Contents is everything an artifact holds, with the index already built.
// The builder subpackage produces it from engine entries; Encode lays it out.
type Contents struct {
	Name string
	// ListConfig is the resolved engine ListConfig exactly as json.Marshal
	// writes it (what engine List.Config returns, marshaled).
	ListConfig     json.RawMessage
	AnalyzerDigest string // engine.AnalyzerSpecDigest of the resolved analyzer
	ConfigDigest   string // engine List.ConfigDigest

	Records []Record // base order: ordinal i is Records[i]

	// Exactly one index, matching the configured mode, built from Records
	// in order.
	Ngram *ngram.Index
	Exact *exact.Index
	// ExactCompanion is the exact index over the same keys and ordinals
	// that an ngram list configured with exact_companion carries beside
	// its ngram index; it must be set exactly when the config says so.
	ExactCompanion *exact.Index

	// Loss counters of the build (engine List.BuildStats).
	DroppedKeys    int
	KeylessEntries int

	// Provenance, recorded verbatim.
	Source      string
	CreatedAt   string
	InputSHA256 string
	Collapsed   int
}

// Summary is what Encode established about the file it produced.
type Summary struct {
	BaseID          string // logical base identity, computed from the records
	ContainerSHA256 string // SHA-256 of the whole file
	Entries         int
	Keys            int64
	Size            int
}

// configDoc is the config section's canonical JSON.
type configDoc struct {
	Name           string          `json:"name"`
	AnalyzerDigest string          `json:"analyzer_digest"`
	ConfigDigest   string          `json:"config_digest"`
	List           json.RawMessage `json:"list"`
}

// BuildInfo is the build section: declared provenance and counts.
type BuildInfo struct {
	BaseID         string `json:"base_id"`
	Entries        int    `json:"entries"`
	Keys           int64  `json:"keys"`
	DroppedKeys    int    `json:"dropped_keys"`
	KeylessEntries int    `json:"keyless_entries"`
	Collapsed      int    `json:"collapsed,omitempty"`
	Source         string `json:"source,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	InputSHA256    string `json:"input_sha256,omitempty"`
}

// canonEntry mirrors engine.Entry field for field and tag for tag, so that
// json.Marshal produces the engine's base line byte for byte. The builder
// tests pin the equivalence against engine.Entry and the engine store.
type canonEntry struct {
	ID      string          `json:"id"`
	Keys    []string        `json:"keys"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// CanonicalPayload returns the bytes json.Marshal writes for p inside an
// entry's base line: compacted, with <, >, &, U+2028 and U+2029 escaped.
// Empty input stays empty (the field is omitted). Invalid JSON is an error,
// as it is for the engine's base writer.
func CanonicalPayload(p []byte) ([]byte, error) {
	if len(p) == 0 {
		return nil, nil
	}
	c, err := json.Marshal(json.RawMessage(p))
	if err != nil {
		return nil, fmt.Errorf("prepared: payload: %w", err)
	}
	return c, nil
}

// lineHasher accumulates the logical base identity.
type lineHasher struct {
	h    hash.Hash
	keys []string
}

func newLineHasher() *lineHasher { return &lineHasher{h: sha256.New()} }

// add hashes one record's canonical line and returns it.
func (lh *lineHasher) add(id string, keys []string, payload []byte) ([]byte, error) {
	line, err := json.Marshal(&canonEntry{ID: id, Keys: keys, Payload: payload})
	if err != nil {
		return nil, err
	}
	if len(line)+1 > MaxLine {
		return nil, fmt.Errorf("record %q: canonical line is %d bytes, over the %d-byte base-line bound", id, len(line)+1, MaxLine)
	}
	lh.h.Write(line)
	lh.h.Write([]byte{'\n'})
	return line, nil
}

func (lh *lineHasher) sum() string { return hex.EncodeToString(lh.h.Sum(nil)) }

// Encode lays out c as a complete artifact and returns its bytes, starting
// at a 32-byte aligned address.
func Encode(c *Contents) ([]byte, *Summary, error) {
	if err := checkHost(); err != nil {
		return nil, nil, err
	}
	if !ValidListName(c.Name) {
		return nil, nil, fmt.Errorf("prepared: list name %q does not match %s", c.Name, listNameRE)
	}
	lc, err := parseListConfig(c.ListConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("prepared: list config: %w", err)
	}
	if !isHex64(c.AnalyzerDigest) || !isHex64(c.ConfigDigest) {
		return nil, nil, fmt.Errorf("prepared: analyzer and config digests must be 64 lowercase hex digits")
	}
	if len(c.Records) > math.MaxUint32 {
		return nil, nil, fmt.Errorf("prepared: %d records exceed the uint32 ordinal space", len(c.Records))
	}
	if c.DroppedKeys < 0 || c.KeylessEntries < 0 || c.KeylessEntries > len(c.Records) || c.Collapsed < 0 {
		return nil, nil, fmt.Errorf("prepared: impossible build counters")
	}

	var index, companion []byte
	switch lc.Match.Mode {
	case "ngram":
		if c.Ngram == nil || c.Exact != nil {
			return nil, nil, fmt.Errorf("prepared: ngram list needs exactly an ngram index")
		}
		cfg := c.Ngram.Cfg()
		if !equalInts(cfg.Grams, lc.Match.Grams) || cfg.StripSpaces != lc.Match.StripSpaces {
			return nil, nil, fmt.Errorf("prepared: ngram index config %+v disagrees with the list config", cfg)
		}
		if index, err = encodeNgram(c.Ngram, len(c.Records)); err != nil {
			return nil, nil, err
		}
		switch {
		case lc.Match.ExactCompanion && c.ExactCompanion == nil:
			return nil, nil, fmt.Errorf("prepared: a list configured with exact_companion needs its exact companion index")
		case !lc.Match.ExactCompanion && c.ExactCompanion != nil:
			return nil, nil, fmt.Errorf("prepared: an exact companion index needs exact_companion in the list config")
		case c.ExactCompanion != nil:
			if companion, err = encodeExact(c.ExactCompanion, len(c.Records)); err != nil {
				return nil, nil, err
			}
		}
	case "exact":
		if c.Exact == nil || c.Ngram != nil || c.ExactCompanion != nil {
			return nil, nil, fmt.Errorf("prepared: exact list needs exactly an exact index")
		}
		if index, err = encodeExact(c.Exact, len(c.Records)); err != nil {
			return nil, nil, err
		}
	}

	records, keys, baseID, err := encodeRecords(c.Records)
	if err != nil {
		return nil, nil, err
	}
	ids, err := encodeIDs(c.Records)
	if err != nil {
		return nil, nil, err
	}
	cfgJSON, err := json.Marshal(&configDoc{Name: c.Name, AnalyzerDigest: c.AnalyzerDigest, ConfigDigest: c.ConfigDigest, List: c.ListConfig})
	if err != nil {
		return nil, nil, err
	}
	buildJSON, err := json.Marshal(&BuildInfo{
		BaseID: baseID, Entries: len(c.Records), Keys: keys,
		DroppedKeys: c.DroppedKeys, KeylessEntries: c.KeylessEntries, Collapsed: c.Collapsed,
		Source: c.Source, CreatedAt: c.CreatedAt, InputSHA256: c.InputSHA256,
	})
	if err != nil {
		return nil, nil, err
	}
	if len(cfgJSON) > MaxMetaSection || len(buildJSON) > MaxMetaSection {
		return nil, nil, fmt.Errorf("prepared: config or build section over %d bytes", MaxMetaSection)
	}
	indexKind := KindNgram
	if lc.Match.Mode == "exact" {
		indexKind = KindExact
	}
	bodies := []struct {
		kind uint32
		data []byte
	}{
		{KindConfig, cfgJSON},
		{KindBuild, buildJSON},
		{KindRecords, records},
		{KindIDs, ids},
		{indexKind, index},
	}
	if companion != nil {
		bodies = append(bodies, struct {
			kind uint32
			data []byte
		}{KindExactCompanion, companion})
	}
	secs := make([]section, len(bodies))
	off, _ := align32(uint64(headerSize + len(bodies)*tableEntrySize + digestSize))
	for i, b := range bodies {
		secs[i] = section{kind: b.kind, flags: flagRequired, off: off, length: uint64(len(b.data)), sum: sha256.Sum256(b.data)}
		var ok bool
		if off, ok = align32(secs[i].end()); !ok {
			return nil, nil, fmt.Errorf("prepared: artifact too large")
		}
	}
	total := secs[len(secs)-1].end()
	if total > math.MaxInt {
		return nil, nil, fmt.Errorf("prepared: artifact too large")
	}
	out := AlignedBuffer(int(total))
	copy(out, encodeHead(secs))
	index, companion, records, ids = nil, nil, nil, nil
	for i, b := range bodies {
		copy(out[secs[i].off:], b.data)
		bodies[i].data = nil // each section buffer becomes collectable once copied
	}
	sum := sha256.Sum256(out)
	return out, &Summary{
		BaseID: baseID, ContainerSHA256: hex.EncodeToString(sum[:]),
		Entries: len(c.Records), Keys: keys, Size: len(out),
	}, nil
}

// encodeRecords builds the records section and the logical base ID.
func encodeRecords(recs []Record) (sec []byte, keys int64, baseID string, err error) {
	var nKeys, blob uint64
	for i := range recs {
		r := &recs[i]
		if r.ID == "" {
			return nil, 0, "", fmt.Errorf("prepared: record %d has an empty ID", i)
		}
		// Never coerce: json.Marshal would silently replace invalid bytes
		// with U+FFFD, and the base ID would then not bind the stored bytes.
		if !utf8.ValidString(r.ID) {
			return nil, 0, "", fmt.Errorf("prepared: record %d: ID %q is not valid UTF-8", i, r.ID)
		}
		for j, k := range r.Keys {
			if !utf8.ValidString(k) {
				return nil, 0, "", fmt.Errorf("prepared: record %d (ID %q): key %d %q is not valid UTF-8", i, r.ID, j, k)
			}
		}
		nKeys += uint64(len(r.Keys))
		blob += uint64(len(r.ID)) + uint64(len(r.Payload))
		for _, k := range r.Keys {
			blob += uint64(len(k))
		}
	}
	n := uint64(len(recs))
	size := 8 + 48*n + 8 + 16*nKeys + blob
	if size > math.MaxInt {
		return nil, 0, "", fmt.Errorf("prepared: records section too large")
	}
	sec = make([]byte, size)
	binary.LittleEndian.PutUint64(sec, n)
	rows := sec[8:]
	binary.LittleEndian.PutUint64(sec[8+48*n:], nKeys)
	keyRows := sec[16+48*n:]
	blobAt := 16 + 48*n + 16*nKeys
	b := sec[blobAt:]
	lh := newLineHasher()
	var cur, kIdx uint64
	put := func(s string) uint64 {
		copy(b[cur:], s)
		at := cur
		cur += uint64(len(s))
		return at
	}
	putBytes := func(p []byte) uint64 {
		copy(b[cur:], p)
		at := cur
		cur += uint64(len(p))
		return at
	}
	for i := range recs {
		r := &recs[i]
		canon, err := CanonicalPayload(r.Payload)
		if err != nil {
			return nil, 0, "", fmt.Errorf("prepared: record %q: %w", r.ID, err)
		}
		if !bytes.Equal(canon, r.Payload) {
			return nil, 0, "", fmt.Errorf("prepared: record %q: payload is not canonical (use CanonicalPayload)", r.ID)
		}
		if _, err := lh.add(r.ID, r.Keys, r.Payload); err != nil {
			return nil, 0, "", fmt.Errorf("prepared: %w", err)
		}
		row := rows[48*i:]
		binary.LittleEndian.PutUint64(row[0:], put(r.ID))
		binary.LittleEndian.PutUint64(row[8:], uint64(len(r.ID)))
		binary.LittleEndian.PutUint64(row[16:], kIdx)
		kc := uint64(len(r.Keys))
		if r.Keys == nil {
			kc |= keysNullFlag
		}
		binary.LittleEndian.PutUint64(row[24:], kc)
		for _, k := range r.Keys {
			kr := keyRows[16*kIdx:]
			binary.LittleEndian.PutUint64(kr[0:], put(k))
			binary.LittleEndian.PutUint64(kr[8:], uint64(len(k)))
			kIdx++
		}
		binary.LittleEndian.PutUint64(row[32:], putBytes(r.Payload))
		binary.LittleEndian.PutUint64(row[40:], uint64(len(r.Payload)))
	}
	return sec, int64(nKeys), lh.sum(), nil
}

// encodeIDs builds the ids section: ordinals in strictly increasing ID
// order. A duplicate ID is an error.
func encodeIDs(recs []Record) ([]byte, error) {
	ords := make([]uint32, len(recs))
	for i := range ords {
		ords[i] = uint32(i)
	}
	sort.Slice(ords, func(a, b int) bool { return recs[ords[a]].ID < recs[ords[b]].ID })
	sec := make([]byte, 8+8*len(recs))
	binary.LittleEndian.PutUint64(sec, uint64(len(recs)))
	for i, o := range ords {
		if i > 0 && recs[ords[i-1]].ID == recs[o].ID {
			return nil, fmt.Errorf("prepared: duplicate record ID %q", recs[o].ID)
		}
		binary.LittleEndian.PutUint64(sec[8+8*i:], uint64(o))
	}
	return sec, nil
}

// encodeNgram builds the ngram section, freezing each bitmap in place.
func encodeNgram(idx *ngram.Index, entries int) ([]byte, error) {
	if err := frozenSupported(); err != nil {
		return nil, err
	}
	post := idx.Postings()
	grams := make([]string, 0, len(post))
	var gramBlob uint64
	for g, bm := range post {
		if g == "" {
			return nil, fmt.Errorf("prepared: ngram index holds an empty gram")
		}
		if bm.IsEmpty() {
			return nil, fmt.Errorf("prepared: ngram index holds an empty bitmap for gram %q", g)
		}
		grams = append(grams, g)
		gramBlob += uint64(len(g))
	}
	sort.Strings(grams)
	if uint64(idx.NumOrds()) > uint64(entries) {
		return nil, fmt.Errorf("prepared: ngram index has %d ordinals for %d records", idx.NumOrds(), entries)
	}
	g := uint64(len(grams))
	head := 8 + 32*g + 16
	bmStart, _ := align32(head + gramBlob)
	offs := make([]uint64, len(grams))
	sizes := make([]uint64, len(grams))
	var cur uint64
	for i, gr := range grams {
		cur, _ = align32(cur)
		offs[i] = cur
		sz, err := frozenSize(post[gr])
		if err != nil {
			return nil, err
		}
		sizes[i] = sz
		cur += sizes[i]
	}
	total := bmStart + cur
	if total > math.MaxInt {
		return nil, fmt.Errorf("prepared: ngram section too large")
	}
	// Aligned so FreezeTo writes its uint64 words through aligned pointers;
	// the section's file offset is aligned too, so these bytes land at the
	// same alignment in the file.
	sec := AlignedBuffer(int(total))
	binary.LittleEndian.PutUint64(sec, g)
	binary.LittleEndian.PutUint64(sec[8+32*g:], uint64(idx.NumOrds()))
	binary.LittleEndian.PutUint64(sec[16+32*g:], gramBlob)
	var gcur uint64
	for i, gr := range grams {
		row := sec[8+32*i:]
		binary.LittleEndian.PutUint64(row[0:], gcur)
		binary.LittleEndian.PutUint64(row[8:], uint64(len(gr)))
		binary.LittleEndian.PutUint64(row[16:], offs[i])
		binary.LittleEndian.PutUint64(row[24:], sizes[i])
		copy(sec[head+gcur:], gr)
		gcur += uint64(len(gr))
		dst := sec[bmStart+offs[i] : bmStart+offs[i]+sizes[i]]
		n, err := freezeTo(post[gr], dst)
		if err != nil {
			return nil, fmt.Errorf("prepared: freezing gram %q: %w", gr, err)
		}
		if uint64(n) != sizes[i] {
			return nil, fmt.Errorf("prepared: frozen gram %q is %d bytes, expected %d", gr, n, sizes[i])
		}
	}
	return sec, nil
}

// encodeExact builds the exact section from the index's sorted keys.
func encodeExact(idx *exact.Index, entries int) ([]byte, error) {
	keys := idx.SortedKeys()
	var arena, postings uint64
	for _, k := range keys {
		if len(k) > math.MaxUint32 {
			return nil, fmt.Errorf("prepared: exact key too long")
		}
		arena += uint64(len(k))
		postings += uint64(len(idx.Lookup(k)))
	}
	if uint64(idx.NumOrds()) > uint64(entries) {
		return nil, fmt.Errorf("prepared: exact index has %d ordinals for %d records", idx.NumOrds(), entries)
	}
	k := uint64(len(keys))
	size := 24 + 8*k + 4*postings + arena
	if size > math.MaxInt {
		return nil, fmt.Errorf("prepared: exact section too large")
	}
	sec := make([]byte, size)
	binary.LittleEndian.PutUint64(sec[0:], k)
	binary.LittleEndian.PutUint64(sec[8:], postings)
	binary.LittleEndian.PutUint64(sec[16:], arena)
	klAt, rlAt, pAt := uint64(24), 24+4*k, 24+8*k
	aAt := pAt + 4*postings
	for i, key := range keys {
		run := idx.Lookup(key)
		binary.LittleEndian.PutUint32(sec[klAt+4*uint64(i):], uint32(len(key)))
		binary.LittleEndian.PutUint32(sec[rlAt+4*uint64(i):], uint32(len(run)))
		for _, o := range run {
			binary.LittleEndian.PutUint32(sec[pAt:], o)
			pAt += 4
		}
		copy(sec[aAt:], key)
		aAt += uint64(len(key))
	}
	return sec, nil
}

// listConfigMirror is the part of the engine's ListConfig the format itself
// depends on. The rest is the engine's to validate (builder.ListConfig).
type listConfigMirror struct {
	Match struct {
		Mode           string `json:"mode"`
		Grams          []int  `json:"grams"`
		StripSpaces    bool   `json:"strip_spaces"`
		ExactCompanion bool   `json:"exact_companion"`
	} `json:"match"`
}

func parseListConfig(raw []byte) (*listConfigMirror, error) {
	var lc listConfigMirror
	if err := json.Unmarshal(raw, &lc); err != nil {
		return nil, err
	}
	switch lc.Match.Mode {
	case "ngram":
		if len(lc.Match.Grams) == 0 {
			return nil, fmt.Errorf("ngram mode without resolved gram sizes")
		}
		for _, g := range lc.Match.Grams {
			if g < 1 || g > ngram.MaxGramSize {
				return nil, fmt.Errorf("gram size %d out of range [1, %d]", g, ngram.MaxGramSize)
			}
		}
	case "exact":
		if lc.Match.ExactCompanion {
			return nil, fmt.Errorf("exact_companion applies to ngram mode only")
		}
	default:
		return nil, fmt.Errorf("unknown match mode %q", lc.Match.Mode)
	}
	return &lc, nil
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func equalInts(a, b []int) bool {
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
