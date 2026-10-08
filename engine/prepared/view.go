package prepared

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"unicode/utf8"
	"unsafe"

	"github.com/RoaringBitmap/roaring"
	"github.com/kurn-dev/kurn/engine/exact"
	"github.com/kurn-dev/kurn/engine/ngram"
)

// Config is the decoded config section.
type Config struct {
	Name           string
	Mode           string // "ngram" | "exact"
	Grams          []int  // resolved gram sizes (ngram mode)
	StripSpaces    bool
	ExactCompanion bool // ngram mode: the artifact also holds an exact companion section
	AnalyzerDigest string
	ConfigDigest   string
	// ListConfig is the resolved engine ListConfig as canonical JSON (a
	// copy). The engine decodes and re-validates it (see
	// builder.ListConfig); this package checks only the fields the format
	// depends on.
	ListConfig json.RawMessage
}

// View is a structurally validated artifact over a caller-owned buffer.
// Every accessor returns views into that buffer; see the package comment
// for the borrowing contract. A View is immutable and safe for concurrent
// use.
type View struct {
	buf   []byte
	tab   *table
	cfg   Config
	build BuildInfo

	n       int    // records
	rows    []byte // n × 48
	nKeys   int
	keyRows []byte // nKeys × 16
	blob    []byte
	ids     []byte // n × 8 ordinals

	ng *ngramView
	ex *exactView // the exact index section (exact mode)
	xc *exactView // the exact companion section (ngram mode with exact_companion)
}

type ngramView struct {
	numOrds  uint32
	g        int
	rows     []byte // g × 32
	gramBlob []byte
	bmBlob   []byte
}

type exactView struct {
	k, p     int
	keyLens  []byte // k × u32
	runLens  []byte // k × u32
	postings []byte // p × u32, 4-byte aligned in memory
	arena    []byte
}

// NewView parses buf as an artifact, running every structural check (not
// the digests: see VerifyBytes). buf must start at a 32-byte aligned
// address and hold exactly one artifact. The View borrows buf.
func NewView(buf []byte) (*View, error) {
	if err := checkHost(); err != nil {
		return nil, err
	}
	if !isAligned(buf) {
		return nil, ErrUnaligned
	}
	hl, err := headLen(buf)
	if err != nil {
		return nil, err
	}
	if hl > len(buf) {
		return nil, malformed("truncated section table: file has %d bytes, header and table need %d", len(buf), hl)
	}
	tab, err := parseTable(buf[:hl], uint64(len(buf)))
	if err != nil {
		return nil, err
	}
	v := &View{buf: buf, tab: tab}
	if err := v.checkGaps(); err != nil {
		return nil, err
	}
	body := func(kind uint32) []byte {
		s, _ := tab.find(kind)
		return buf[s.off:s.end():s.end()]
	}
	if v.cfg, err = parseConfig(body(KindConfig)); err != nil {
		return nil, err
	}
	if v.build, err = parseBuild(body(KindBuild)); err != nil {
		return nil, err
	}
	if err := v.parseRecords(body(KindRecords)); err != nil {
		return nil, err
	}
	if v.build.Entries != v.n {
		return nil, malformed("build section declares %d entries, records section holds %d", v.build.Entries, v.n)
	}
	if v.build.Keys != int64(v.nKeys) {
		return nil, malformed("build section declares %d keys, records section holds %d", v.build.Keys, v.nKeys)
	}
	if v.build.KeylessEntries > v.n {
		return nil, malformed("build section declares %d keyless of %d entries", v.build.KeylessEntries, v.n)
	}
	if err := v.parseIDs(body(KindIDs)); err != nil {
		return nil, err
	}
	_, hasNgram := tab.find(KindNgram)
	if hasNgram != (v.cfg.Mode == "ngram") {
		return nil, malformed("config mode %q but the index section is %s", v.cfg.Mode, map[bool]string{true: "ngram", false: "exact"}[hasNgram])
	}
	if hasNgram {
		err = v.parseNgram(body(KindNgram))
	} else {
		err = v.parseExact(body(KindExact))
	}
	if err != nil {
		return nil, err
	}
	_, hasCompanion := tab.find(KindExactCompanion)
	if hasCompanion != v.cfg.ExactCompanion {
		return nil, malformed("config exact_companion %v but the exact companion section is %s", v.cfg.ExactCompanion, map[bool]string{true: "present", false: "absent"}[hasCompanion])
	}
	if hasCompanion {
		if v.xc, err = v.parseExactSection(body(KindExactCompanion), "exact companion"); err != nil {
			return nil, err
		}
	}
	return v, nil
}

// checkGaps requires every byte outside the header and sections to be zero.
func (v *View) checkGaps() error {
	prev := v.tab.headEnd
	for _, s := range v.tab.sections {
		for _, c := range v.buf[prev:s.off] {
			if c != 0 {
				return malformed("nonzero byte in the gap before section kind %d", s.kind)
			}
		}
		prev = s.end()
	}
	return nil
}

func parseConfig(b []byte) (Config, error) {
	var c Config
	if len(b) > MaxMetaSection {
		return c, malformed("config section is %d bytes, over %d", len(b), MaxMetaSection)
	}
	var doc configDoc
	if err := strictJSON(b, &doc); err != nil {
		return c, malformed("config section: %v", err)
	}
	if !ValidListName(doc.Name) {
		return c, malformed("config section: list name %q does not match %s", doc.Name, listNameRE)
	}
	if !isHex64(doc.AnalyzerDigest) || !isHex64(doc.ConfigDigest) {
		return c, malformed("config section: digests must be 64 lowercase hex digits")
	}
	lc, err := parseListConfig(doc.List)
	if err != nil {
		return c, malformed("config section: list: %v", err)
	}
	return Config{
		Name: doc.Name, Mode: lc.Match.Mode, Grams: lc.Match.Grams, StripSpaces: lc.Match.StripSpaces,
		ExactCompanion: lc.Match.ExactCompanion,
		AnalyzerDigest: doc.AnalyzerDigest, ConfigDigest: doc.ConfigDigest, ListConfig: doc.List,
	}, nil
}

func parseBuild(b []byte) (BuildInfo, error) {
	var bi BuildInfo
	if len(b) > MaxMetaSection {
		return bi, malformed("build section is %d bytes, over %d", len(b), MaxMetaSection)
	}
	if err := strictJSON(b, &bi); err != nil {
		return bi, malformed("build section: %v", err)
	}
	if !isHex64(bi.BaseID) {
		return bi, malformed("build section: base_id must be 64 lowercase hex digits")
	}
	if bi.Entries < 0 || bi.Keys < 0 || bi.DroppedKeys < 0 || bi.KeylessEntries < 0 || bi.Collapsed < 0 {
		return bi, malformed("build section: negative count")
	}
	return bi, nil
}

// strictJSON decodes b into v, refusing unknown fields and trailing data,
// and requires b to be exactly what json.Marshal writes for the result.
func strictJSON(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data")
	}
	re, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if !bytes.Equal(re, b) {
		return fmt.Errorf("not canonical JSON")
	}
	return nil
}

// count reads a u64 element count at off and checks that count elements of
// elem bytes fit in the len(b)-off-reserve bytes that follow it.
func count(b []byte, off int, elem uint64, reserve int, what string) (int, error) {
	if off+8 > len(b) {
		return 0, malformed("%s: truncated count", what)
	}
	n := le64(b, off)
	room := uint64(len(b) - off - 8)
	if uint64(reserve) > room {
		return 0, malformed("%s: truncated", what)
	}
	room -= uint64(reserve)
	if elem > 0 && n > room/elem {
		return 0, malformed("%s: count %d does not fit in %d bytes", what, n, room)
	}
	if n > math.MaxUint32 {
		return 0, malformed("%s: count %d exceeds the uint32 ordinal space", what, n)
	}
	return int(n), nil
}

func (v *View) parseRecords(b []byte) error {
	n, err := count(b, 0, 48, 8, "records")
	if err != nil {
		return err
	}
	rowsEnd := 8 + 48*n
	nk, err := count(b, rowsEnd, 16, 0, "records key table")
	if err != nil {
		return err
	}
	keysEnd := rowsEnd + 8 + 16*nk
	v.n, v.rows = n, b[8:rowsEnd:rowsEnd]
	v.nKeys, v.keyRows = nk, b[rowsEnd+8:keysEnd:keysEnd]
	v.blob = b[keysEnd:len(b):len(b)]
	blobLen := uint64(len(v.blob))
	// Canonical packing: every offset is the running cursor, so each is in
	// bounds exactly when the running sum is.
	var cur, kIdx uint64
	// IDs and keys must be valid UTF-8: json.Marshal replaces each invalid
	// byte with U+FFFD, so for invalid strings the canonical line — and
	// therefore the base ID — would not bind the bytes the View serves.
	take := func(off, length uint64, what string, i int) error {
		if off != cur {
			return malformed("record %d: %s offset %d, expected %d", i, what, off, cur)
		}
		if length > blobLen-cur {
			return malformed("record %d: %s length %d overruns the blob", i, what, length)
		}
		if what != "payload" && !utf8.Valid(v.blob[cur:cur+length]) {
			return malformed("record %d: %s is not valid UTF-8", i, what)
		}
		cur += length
		return nil
	}
	for i := 0; i < n; i++ {
		r := v.rows[48*i:]
		idLen := le64(r, 8)
		if idLen == 0 {
			return malformed("record %d: empty ID", i)
		}
		if err := take(le64(r, 0), idLen, "id", i); err != nil {
			return err
		}
		ks, kc := le64(r, 16), le64(r, 24)
		if kc&keysNullFlag != 0 && kc != keysNullFlag {
			return malformed("record %d: null key list with a nonzero count", i)
		}
		kc &^= keysNullFlag
		if ks != kIdx {
			return malformed("record %d: key range starts at %d, expected %d", i, ks, kIdx)
		}
		if kc > uint64(nk)-kIdx {
			return malformed("record %d: key count %d overruns the key table", i, kc)
		}
		for j := uint64(0); j < kc; j++ {
			kr := v.keyRows[16*(kIdx+j):]
			if err := take(le64(kr, 0), le64(kr, 8), "key", i); err != nil {
				return err
			}
		}
		kIdx += kc
		if err := take(le64(r, 32), le64(r, 40), "payload", i); err != nil {
			return err
		}
	}
	if kIdx != uint64(nk) {
		return malformed("records use %d keys, key table holds %d", kIdx, nk)
	}
	if cur != blobLen {
		return malformed("records use %d blob bytes, blob holds %d", cur, blobLen)
	}
	return nil
}

func (v *View) parseIDs(b []byte) error {
	n, err := count(b, 0, 8, 0, "ids")
	if err != nil {
		return err
	}
	if n != v.n {
		return malformed("ids section holds %d entries, records hold %d", n, v.n)
	}
	if len(b) != 8+8*n {
		return malformed("ids section is %d bytes, expected %d", len(b), 8+8*n)
	}
	v.ids = b[8:]
	// Strictly increasing IDs over in-range ordinals: distinct IDs imply
	// distinct ordinals, so n of them form a permutation.
	var prev string
	for i := 0; i < n; i++ {
		o := le64(v.ids, 8*i)
		if o >= uint64(n) {
			return malformed("ids[%d]: ordinal %d out of range", i, o)
		}
		id := v.ID(int(o))
		if i > 0 && id <= prev {
			return malformed("ids[%d]: IDs not strictly increasing (duplicate or unsorted)", i)
		}
		prev = id
	}
	return nil
}

func (v *View) parseNgram(b []byte) error {
	if err := frozenSupported(); err != nil {
		return err
	}
	g, err := count(b, 0, 32, 16, "ngram")
	if err != nil {
		return err
	}
	head := 8 + 32*g
	numOrds := le64(b, head)
	gramBlobLen := le64(b, head+8)
	gramAt := uint64(head + 16)
	if gramBlobLen > uint64(len(b))-gramAt {
		return malformed("ngram: gram blob length %d overruns the section", gramBlobLen)
	}
	bmStart, _ := align32(gramAt + gramBlobLen)
	if bmStart > uint64(len(b)) {
		return malformed("ngram: section ends inside the bitmap padding")
	}
	for _, c := range b[gramAt+gramBlobLen : bmStart] {
		if c != 0 {
			return malformed("ngram: nonzero padding before the bitmap blob")
		}
	}
	if numOrds > uint64(v.n) {
		return malformed("ngram: %d ordinals for %d records", numOrds, v.n)
	}
	nv := &ngramView{
		numOrds:  uint32(numOrds),
		g:        g,
		rows:     b[8:head:head],
		gramBlob: b[gramAt : gramAt+gramBlobLen : gramAt+gramBlobLen],
		bmBlob:   b[bmStart:len(b):len(b)],
	}
	bmLen := uint64(len(nv.bmBlob))
	var gcur, bcur uint64
	var prev string
	var maxOrd uint32
	for i := 0; i < g; i++ {
		r := nv.rows[32*i:]
		goff, glen, boff, blen := le64(r, 0), le64(r, 8), le64(r, 16), le64(r, 24)
		if goff != gcur || glen == 0 || glen > gramBlobLen-gcur {
			return malformed("ngram: gram %d at %d+%d breaks the packed gram blob", i, goff, glen)
		}
		gcur += glen
		gram := unsafe.String(&nv.gramBlob[goff], int(glen))
		if i > 0 && gram <= prev {
			return malformed("ngram: grams not strictly increasing at %d", i)
		}
		prev = gram
		want, _ := align32(bcur)
		if boff%Align != 0 {
			return malformed("ngram: bitmap %d offset %d is not %d-byte aligned", i, boff, Align)
		}
		if boff != want || want > bmLen || blen > bmLen-want {
			return malformed("ngram: bitmap %d at %d+%d breaks the packed bitmap blob", i, boff, blen)
		}
		for _, c := range nv.bmBlob[bcur:boff] {
			if c != 0 {
				return malformed("ngram: nonzero padding before bitmap %d", i)
			}
		}
		fi, err := checkFrozen(nv.bmBlob[boff : boff+blen])
		if err != nil {
			return fmt.Errorf("ngram: bitmap %d (gram %q): %w", i, gram, err)
		}
		if fi.max >= nv.numOrds {
			return malformed("ngram: bitmap %d holds ordinal %d, numOrds is %d", i, fi.max, nv.numOrds)
		}
		if fi.max > maxOrd {
			maxOrd = fi.max
		}
		bcur = boff + blen
	}
	if gcur != gramBlobLen {
		return malformed("ngram: grams use %d of %d gram blob bytes", gcur, gramBlobLen)
	}
	if bcur != bmLen {
		return malformed("ngram: bitmaps use %d of %d bitmap blob bytes", bcur, bmLen)
	}
	// numOrds is exactly max ordinal + 1, as ngram.Builder.Finish computes.
	var wantOrds uint64
	if g > 0 {
		wantOrds = uint64(maxOrd) + 1
	}
	if numOrds != wantOrds {
		return malformed("ngram: numOrds %d, postings imply %d", numOrds, wantOrds)
	}
	v.ng = nv
	return nil
}

// parseExact parses the exact index section of an exact-mode artifact.
func (v *View) parseExact(b []byte) error {
	ev, err := v.parseExactSection(b, "exact")
	if err != nil {
		return err
	}
	v.ex = ev
	return nil
}

// parseExactSection parses one exact-layout section (the exact index or
// the exact companion); what prefixes its error messages.
func (v *View) parseExactSection(b []byte, what string) (*exactView, error) {
	if len(b) < 24 {
		return nil, malformed(what + ": truncated header")
	}
	k, p, arenaLen := le64(b, 0), le64(b, 8), le64(b, 16)
	room := uint64(len(b) - 24)
	if k > room/8 {
		return nil, malformed(what+": key count %d does not fit", k)
	}
	room -= 8 * k
	if p > room/4 {
		return nil, malformed(what+": postings count %d does not fit", p)
	}
	room -= 4 * p
	if arenaLen != room {
		return nil, malformed(what+": arena length %d, section leaves %d bytes", arenaLen, room)
	}
	if p > math.MaxUint32 {
		return nil, malformed(what+": postings count %d out of range", p)
	}
	ev := &exactView{k: int(k), p: int(p)}
	at := uint64(24)
	ev.keyLens = b[at : at+4*k : at+4*k]
	at += 4 * k
	ev.runLens = b[at : at+4*k : at+4*k]
	at += 4 * k
	ev.postings = b[at : at+4*p : at+4*p]
	at += 4 * p
	ev.arena = b[at:len(b):len(b)]
	var aOff, pOff uint64
	var prev string
	for i := 0; i < ev.k; i++ {
		kl := uint64(le32(ev.keyLens, 4*i))
		rl := uint64(le32(ev.runLens, 4*i))
		if kl == 0 || kl > arenaLen-aOff {
			return nil, malformed(what+": key %d length %d out of bounds", i, kl)
		}
		if rl == 0 || rl > exactMaxRun || rl > p-pOff {
			return nil, malformed(what+": key %d run length %d out of bounds", i, rl)
		}
		key := unsafe.String(&ev.arena[aOff], int(kl))
		if i > 0 && key <= prev {
			return nil, malformed(what+": keys not strictly increasing at %d", i)
		}
		prev = key
		var last uint32
		for j := uint64(0); j < rl; j++ {
			o := le32(ev.postings, int(4*(pOff+j)))
			if uint64(o) >= uint64(v.n) {
				return nil, malformed(what+": key %d holds ordinal %d, records hold %d", i, o, v.n)
			}
			if j > 0 && o <= last {
				return nil, malformed(what+": key %d run not strictly ascending", i)
			}
			last = o
		}
		aOff += kl
		pOff += rl
	}
	if aOff != arenaLen || pOff != p {
		return nil, malformed(what+": keys use %d/%d arena bytes and %d/%d postings", aOff, arenaLen, pOff, p)
	}
	return ev, nil
}

// exactMaxRun mirrors engine/exact's maxRun (2^20-1), the longest run its
// packed locator encodes; exact.Restore refuses longer runs.
const exactMaxRun = 1<<20 - 1

// Config returns the decoded config section.
func (v *View) Config() Config {
	c := v.cfg
	c.Grams = append([]int(nil), c.Grams...)
	return c
}

// Build returns the decoded build section (declared values; VerifyBytes
// establishes the base ID).
func (v *View) Build() BuildInfo { return v.build }

// Version returns the artifact's format version.
func (v *View) Version() (major, minor uint16) { return v.tab.major, v.tab.minor }

// Len returns the number of records.
func (v *View) Len() int { return v.n }

// ID returns record i's ID, a view into the buffer.
func (v *View) ID(i int) string {
	r := v.rows[48*i:]
	return v.str(le64(r, 0), le64(r, 8))
}

// KeyCount returns how many keys record i has.
func (v *View) KeyCount(i int) int { return int(le64(v.rows[48*i:], 24) &^ keysNullFlag) }

// KeysNull reports whether record i's key list is JSON null (nil).
func (v *View) KeysNull(i int) bool { return le64(v.rows[48*i:], 24) == keysNullFlag }

// Key returns record i's key j, a view into the buffer.
func (v *View) Key(i, j int) string {
	kr := v.keyRows[16*(int(le64(v.rows[48*i:], 16))+j):]
	return v.str(le64(kr, 0), le64(kr, 8))
}

// Keys returns record i's keys in a new slice of string views (nil when the
// stored key list is null).
func (v *View) Keys(i int) []string {
	if v.KeysNull(i) {
		return nil
	}
	return v.AppendKeys(make([]string, 0, v.KeyCount(i)), i)
}

// AppendKeys appends record i's keys to dst.
func (v *View) AppendKeys(dst []string, i int) []string {
	n := v.KeyCount(i)
	for j := 0; j < n; j++ {
		dst = append(dst, v.Key(i, j))
	}
	return dst
}

// Payload returns record i's payload bytes, capped at their own length, or
// nil when it has none.
func (v *View) Payload(i int) []byte {
	r := v.rows[48*i:]
	off, n := le64(r, 32), le64(r, 40)
	if n == 0 {
		return nil
	}
	return v.blob[off : off+n : off+n]
}

func (v *View) str(off, n uint64) string {
	if n == 0 {
		return ""
	}
	return unsafe.String(&v.blob[off], int(n))
}

// IDOrdinal returns the ordinal of the k-th ID in bytewise ID order.
func (v *View) IDOrdinal(k int) int { return int(le64(v.ids, 8*k)) }

// Lookup finds the record ordinal holding id by binary search over the
// ids section.
func (v *View) Lookup(id string) (int, bool) {
	k := sort.Search(v.n, func(k int) bool { return v.ID(v.IDOrdinal(k)) >= id })
	if k < v.n && v.ID(v.IDOrdinal(k)) == id {
		return v.IDOrdinal(k), true
	}
	return 0, false
}

// NumOrds returns the index's ordinal count (max ordinal + 1).
func (v *View) NumOrds() uint32 {
	if v.ng != nil {
		return v.ng.numOrds
	}
	var m uint32
	for i := 0; i < v.ex.p; i++ {
		if o := le32(v.ex.postings, 4*i) + 1; o > m {
			m = o
		}
	}
	return m
}

// GramCount returns the number of grams (0 for exact artifacts).
func (v *View) GramCount() int {
	if v.ng == nil {
		return 0
	}
	return v.ng.g
}

// Gram returns gram i, a view into the buffer.
func (v *View) Gram(i int) string {
	r := v.ng.rows[32*i:]
	off, n := le64(r, 0), le64(r, 8)
	return unsafe.String(&v.ng.gramBlob[off], int(n))
}

// FrozenBitmap returns gram i's frozen bitmap bytes, capped at their length.
func (v *View) FrozenBitmap(i int) []byte {
	r := v.ng.rows[32*i:]
	off, n := le64(r, 16), le64(r, 24)
	return v.ng.bmBlob[off : off+n : off+n]
}

// Postings returns gram i's posting bitmap as a FrozenView over the buffer.
// It must never be mutated (see the package comment).
func (v *View) Postings(i int) (*roaring.Bitmap, error) {
	b := v.FrozenBitmap(i)
	// NewView checked every bitmap in full; the O(containers) trailer
	// check is repeated right before the bytes reach roaring.
	if _, err := checkFrozenTrailer(b); err != nil {
		return nil, err
	}
	if !isAligned(b) {
		return nil, ErrUnaligned
	}
	bm := roaring.New()
	if err := frozenView(bm, b); err != nil {
		return nil, malformed("gram %d: frozen view: %v", i, err)
	}
	return bm, nil
}

// NgramIndex rebuilds the ngram index over FrozenView postings.
func (v *View) NgramIndex() (*ngram.Index, error) {
	if v.ng == nil {
		return nil, fmt.Errorf("prepared: not an ngram artifact")
	}
	post := make(map[string]*roaring.Bitmap, v.ng.g)
	for i := 0; i < v.ng.g; i++ {
		bm, err := v.Postings(i)
		if err != nil {
			return nil, err
		}
		post[v.Gram(i)] = bm
	}
	return ngram.Restore(ngram.Config{Grams: append([]int(nil), v.cfg.Grams...), StripSpaces: v.cfg.StripSpaces}, post, v.ng.numOrds)
}

// ExactParts returns what exact.Restore takes: the key arena (a view), the
// key and run lengths (decoded), and the flat postings (a view).
func (v *View) ExactParts() (arena []byte, keyLens, runLens []int, postings []uint32, err error) {
	if v.ex == nil {
		return nil, nil, nil, nil, fmt.Errorf("prepared: not an exact artifact")
	}
	return exactParts(v.ex)
}

// ExactCompanionParts is ExactParts for the exact companion section of an
// ngram artifact configured with exact_companion.
func (v *View) ExactCompanionParts() (arena []byte, keyLens, runLens []int, postings []uint32, err error) {
	if v.xc == nil {
		return nil, nil, nil, nil, fmt.Errorf("prepared: the artifact holds no exact companion section")
	}
	return exactParts(v.xc)
}

func exactParts(ev *exactView) (arena []byte, keyLens, runLens []int, postings []uint32, err error) {
	keyLens = make([]int, ev.k)
	runLens = make([]int, ev.k)
	for i := range keyLens {
		keyLens[i] = int(le32(ev.keyLens, 4*i))
		runLens[i] = int(le32(ev.runLens, 4*i))
	}
	if ev.p > 0 {
		if uintptr(unsafe.Pointer(&ev.postings[0]))%4 != 0 {
			return nil, nil, nil, nil, ErrUnaligned
		}
		// Little-endian host (checked in NewView) and 4-byte alignment
		// (32-aligned buffer, 32-aligned section, 24 + 8K offset) make the
		// bytes a valid []uint32 in place.
		postings = unsafe.Slice((*uint32)(unsafe.Pointer(&ev.postings[0])), ev.p)
		postings = postings[:ev.p:ev.p]
	} else {
		postings = []uint32{}
	}
	arena = ev.arena
	if len(arena) == 0 {
		arena = []byte{}
	}
	return arena, keyLens, runLens, postings, nil
}

// ExactIndex rebuilds the exact index over the buffer via exact.Restore.
func (v *View) ExactIndex() (*exact.Index, error) {
	arena, kl, rl, post, err := v.ExactParts()
	if err != nil {
		return nil, err
	}
	return exact.Restore(arena, kl, rl, post)
}

// ExactCompanionIndex rebuilds the exact companion index over the buffer
// via exact.Restore.
func (v *View) ExactCompanionIndex() (*exact.Index, error) {
	arena, kl, rl, post, err := v.ExactCompanionParts()
	if err != nil {
		return nil, err
	}
	return exact.Restore(arena, kl, rl, post)
}
