package prepared

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
)

// Level selects how much VerifyBytes establishes beyond structure.
type Level int

const (
	// LevelFull (the default) checks the header digest and every section
	// digest, recomputes the logical base ID from the records, and computes
	// the container digest.
	LevelFull Level = iota
	// LevelStructure runs only the structural checks, which every level
	// runs. It establishes no identity: BaseID and ContainerSHA256 stay
	// empty. It is a diagnostic, not a serving policy.
	LevelStructure
	// LevelTrusted is the operator-trusted serving policy: the operator
	// asserts that the artifact was fully verified before deployment and
	// that its build, delivery and storage are trusted. It runs every
	// structural check, then checks the header and table digest and the
	// recorded digest of every section except the three content sections
	// (records, ids, and the one index section present), and requires
	// every non-empty payload to be valid JSON (json.Valid; a nil or empty
	// payload is well-formed). It skips the content work: the records,
	// ids and index section digests, the base-ID recomputation with its
	// canonical-payload check, and the container digest. BaseID and
	// ContainerSHA256 stay empty; DeclaredBaseID is the build section's.
	//
	// Assurance boundary: trusted verification cannot detect content
	// corruption or misrepresentation that stays structurally valid (an
	// altered payload, ID or posting, or records that no longer hash to
	// the declared base ID), which LevelFull detects. The header digest
	// detects inconsistency between the header and the section table, not
	// authenticity: unsigned embedded digests and read-only permissions
	// prove nothing about who produced the bytes. Trusted verification is
	// not size-independent: record, index and gap walks, the payload
	// syntax check and the hashing of every non-content section (including
	// unknown optional sections, which are not size-bounded) all remain.
	LevelTrusted
)

func (l Level) String() string {
	switch l {
	case LevelFull:
		return "full"
	case LevelStructure:
		return "structure"
	case LevelTrusted:
		return "trusted"
	}
	return fmt.Sprintf("Level(%d)", int(l))
}

// Verified is what verification established about an artifact.
type Verified struct {
	Level        Level
	Major, Minor uint16
	Name         string
	Mode         string
	Entries      int
	Keys         int64
	Size         int64
	// ExactCompanion is true when the artifact holds an exact companion
	// section (an ngram list configured with exact_companion).
	ExactCompanion bool

	// BaseID is the logical base identity recomputed from the records
	// (LevelFull only; empty otherwise, never filled from declared values).
	BaseID string
	// ContainerSHA256 is the SHA-256 of the whole file (LevelFull only;
	// empty otherwise).
	ContainerSHA256 string

	// HeaderDigestVerified is true when the header and table digest was
	// checked (LevelFull and LevelTrusted).
	HeaderDigestVerified bool
	// ContentVerified is true only at LevelFull: every section digest,
	// the canonical payloads and the logical base ID were recomputed.
	ContentVerified bool

	// Declared values, carried for status output. DeclaredBaseID equals
	// BaseID whenever LevelFull succeeded. The config and analyzer digests
	// are the engine's to recompute when it loads the configuration.
	DeclaredBaseID string
	ConfigDigest   string
	AnalyzerDigest string
}

// Info is declared, unverified metadata (see Inspect).
type Info struct {
	Format         string // e.g. "KURNART1 1.0"
	Major, Minor   uint16
	Name           string
	Mode           string
	ExactCompanion bool // declared in the configuration, not verified
	Entries        int
	BaseID         string // declared, not verified
	ConfigDigest   string // declared, not verified
	AnalyzerDigest string // declared, not verified
	Size           int64
}

// VerifyBytes validates buf as an artifact at the given level and returns
// the View over it together with what was established. Structural
// validation always runs first. See the Level constants for what each
// level checks and skips.
func VerifyBytes(buf []byte, level Level) (*View, *Verified, error) {
	if level != LevelFull && level != LevelStructure && level != LevelTrusted {
		return nil, nil, fmt.Errorf("prepared: unknown verification level %d", level)
	}
	v, err := NewView(buf)
	if err != nil {
		return nil, nil, err
	}
	out := &Verified{
		Level: level, Major: v.tab.major, Minor: v.tab.minor,
		Name: v.cfg.Name, Mode: v.cfg.Mode, ExactCompanion: v.xc != nil, Entries: v.n, Keys: int64(v.nKeys), Size: int64(len(buf)),
		DeclaredBaseID: v.build.BaseID, ConfigDigest: v.cfg.ConfigDigest, AnalyzerDigest: v.cfg.AnalyzerDigest,
	}
	if level == LevelStructure {
		return v, out, nil
	}
	// Retained at LevelFull and LevelTrusted: the header and table digest.
	hd := v.tab.headEnd - digestSize
	if sha256.Sum256(buf[:hd]) != v.tab.digest {
		return nil, nil, integrity("header and section table digest mismatch")
	}
	// Every section digest at LevelFull; at LevelTrusted every section's
	// except the three content sections.
	for _, s := range v.tab.sections {
		if level == LevelTrusted && v.contentSection(s.kind) {
			continue // skipped at LevelTrusted
		}
		if sha256.Sum256(buf[s.off:s.end()]) != s.sum {
			return nil, nil, integrity("section kind %d digest mismatch", s.kind)
		}
	}
	out.HeaderDigestVerified = true
	if level == LevelTrusted {
		// Retained at LevelTrusted: payload syntax. Skipped: the base-ID
		// recomputation, the canonical-payload check and the container
		// digest.
		if err := v.checkPayloadSyntax(); err != nil {
			return nil, nil, err
		}
		return v, out, nil
	}
	baseID, err := v.recomputeBaseID()
	if err != nil {
		return nil, nil, err
	}
	if baseID != v.build.BaseID {
		return nil, nil, integrity("records hash to base ID %s, the build section declares %s", baseID, v.build.BaseID)
	}
	sum := sha256.Sum256(buf)
	out.BaseID = baseID
	out.ContainerSHA256 = hex.EncodeToString(sum[:])
	out.ContentVerified = true
	return v, out, nil
}

// contentSection reports whether a section of this kind is one of the
// content sections LevelTrusted does not hash: records, ids, the index
// section present in the file (NewView requires exactly one of ngram and
// exact), and the exact companion section when present. Config, build and
// unknown optional sections are not content sections.
func (v *View) contentSection(kind uint32) bool {
	switch kind {
	case KindRecords, KindIDs:
		return true
	case KindNgram:
		return v.ng != nil
	case KindExact:
		return v.ex != nil
	case KindExactCompanion:
		return v.xc != nil
	}
	return false
}

// checkPayloadSyntax is LevelTrusted's payload rule: a nil or empty payload
// is well-formed and is not passed to json.Valid; every non-empty payload
// must be valid JSON. It checks syntax only, not canonical form.
func (v *View) checkPayloadSyntax() error {
	for i := 0; i < v.n; i++ {
		if p := v.Payload(i); len(p) > 0 && !json.Valid(p) {
			return malformed("record %d: payload is not valid JSON", i)
		}
	}
	return nil
}

// recomputeBaseID re-encodes every record with the canonical encoding and
// hashes the stream. A payload that is not already in canonical form is an
// integrity error: the artifact must serve the exact bytes of the line the
// identity names.
func (v *View) recomputeBaseID() (string, error) {
	lh := newLineHasher()
	// Non-nil from the start: an empty key list re-encodes as [], and only
	// a record flagged null may re-encode as null.
	keys := make([]string, 0, 8)
	for i := 0; i < v.n; i++ {
		keys = v.AppendKeys(keys[:0], i)
		k := keys
		if v.KeysNull(i) {
			k = nil
		}
		p := v.Payload(i)
		c, err := CanonicalPayload(p)
		if err != nil {
			return "", integrity("record %d: %v", i, err)
		}
		if !bytes.Equal(c, p) {
			return "", integrity("record %d: payload is not in canonical form", i)
		}
		if _, err := lh.add(v.ID(i), k, p); err != nil {
			return "", integrity("%v", err)
		}
	}
	return lh.sum(), nil
}

// Verify reads the artifact at path through one descriptor into a
// 32-byte aligned heap buffer and verifies it at level.
func Verify(path string, level Level) (*Verified, error) {
	buf, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	_, out, err := VerifyBytes(buf, level)
	return out, err
}

// ReadFile reads the file at path into a 32-byte aligned buffer.
func ReadFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadAll(f)
}

// ReadAll reads the whole of f (from offset 0, by its current size) into a
// 32-byte aligned buffer. A file that changes size while being read is an
// error.
func ReadAll(f *os.File) ([]byte, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("prepared: %s is not a regular file", f.Name())
	}
	size := fi.Size()
	if size < 0 || size > math.MaxInt-Align {
		return nil, fmt.Errorf("prepared: %s: size %d out of range", f.Name(), size)
	}
	buf := AlignedBuffer(int(size))
	if _, err := io.ReadFull(io.NewSectionReader(f, 0, size), buf); err != nil {
		return nil, fmt.Errorf("prepared: reading %s: %w", f.Name(), err)
	}
	var one [1]byte
	if n, _ := f.ReadAt(one[:], size); n != 0 {
		return nil, fmt.Errorf("prepared: %s grew while being read", f.Name())
	}
	return buf, nil
}

// Inspect returns an artifact's DECLARED metadata without verifying
// anything cryptographically. It reads the header and section table, then
// the two small metadata sections (config and build), and runs the
// structural checks on what it read; it never reads records or index
// sections and never computes a digest. Nothing it returns is verified —
// use Verify for identity.
func Inspect(path string) (*Info, error) {
	if err := checkHost(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("prepared: %s is not a regular file", path)
	}
	size := uint64(fi.Size())
	fixed := make([]byte, headerSize)
	if _, err := f.ReadAt(fixed, 0); err != nil {
		if err == io.EOF {
			return nil, malformed("truncated header: file has %d bytes", size)
		}
		return nil, err
	}
	hl, err := headLen(fixed)
	if err != nil {
		return nil, err
	}
	if uint64(hl) > size {
		return nil, malformed("truncated section table: file has %d bytes, header and table need %d", size, hl)
	}
	head := make([]byte, hl)
	if _, err := f.ReadAt(head, 0); err != nil {
		return nil, err
	}
	tab, err := parseTable(head, size)
	if err != nil {
		return nil, err
	}
	read := func(kind uint32) ([]byte, error) {
		s, _ := tab.find(kind)
		if s.length > MaxMetaSection {
			return nil, malformed("section kind %d is %d bytes, over %d", kind, s.length, MaxMetaSection)
		}
		b := make([]byte, s.length)
		if _, err := f.ReadAt(b, int64(s.off)); err != nil {
			return nil, err
		}
		return b, nil
	}
	cb, err := read(KindConfig)
	if err != nil {
		return nil, err
	}
	cfg, err := parseConfig(cb)
	if err != nil {
		return nil, err
	}
	bb, err := read(KindBuild)
	if err != nil {
		return nil, err
	}
	build, err := parseBuild(bb)
	if err != nil {
		return nil, err
	}
	return &Info{
		Format: fmt.Sprintf("%s %d.%d", Magic, tab.major, tab.minor), Major: tab.major, Minor: tab.minor,
		Name: cfg.Name, Mode: cfg.Mode, ExactCompanion: cfg.ExactCompanion, Entries: build.Entries, BaseID: build.BaseID,
		ConfigDigest: cfg.ConfigDigest, AnalyzerDigest: cfg.AnalyzerDigest, Size: int64(size),
	}, nil
}
