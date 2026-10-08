package prepared

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"unsafe"
)

const (
	// Magic opens every artifact.
	Magic = "KURNART1"
	// MajorVersion and MinorVersion are the format version this package
	// writes. It reads any minor version of MajorVersion whose unknown
	// sections are all optional. 1.1 added the exact companion section
	// (KindExactCompanion), written only for ngram lists configured with
	// exact_companion; a 1.0 reader refuses such an artifact as
	// incompatible (an unknown required section) and reads every other
	// 1.1 artifact.
	MajorVersion uint16 = 1
	MinorVersion uint16 = 1

	// Align is the alignment of every section's file offset, of every
	// embedded bitmap, and of the buffer a View is built over.
	Align = 32

	// MaxSections bounds the section table.
	MaxSections = 64

	// MaxLine is the engine's base-line bound (engine maxLine): a record
	// whose canonical line, newline included, exceeds it could never have
	// been written to base.jsonl, so an artifact may not hold it either.
	MaxLine = 1 << 20

	// MaxMetaSection bounds the config and build sections, which Inspect
	// reads without verification.
	MaxMetaSection = 4 << 20

	headerSize     = 24
	tableEntrySize = 64
	digestSize     = sha256.Size

	flagRequired = 1

	// keysNullFlag marks a record whose key list is JSON null (bit 63 of
	// the key count word).
	keysNullFlag = uint64(1) << 63
)

// Section kinds.
const (
	KindConfig  uint32 = 1
	KindBuild   uint32 = 2
	KindRecords uint32 = 3
	KindIDs     uint32 = 4
	KindNgram   uint32 = 5
	KindExact   uint32 = 6
	// KindExactCompanion is the exact index an ngram list configured with
	// exact_companion carries beside its ngram section: the same layout
	// as KindExact, over the same analyzed keys and ordinals.
	KindExactCompanion uint32 = 7
)

var (
	// ErrMalformed marks a structural error: the bytes do not form a valid
	// artifact, whatever their digests say.
	ErrMalformed = errors.New("prepared: malformed artifact")
	// ErrIncompatible marks an artifact this build cannot read: an unknown
	// major version, unknown header flags, an unknown required section, or
	// a big-endian host.
	ErrIncompatible = errors.New("prepared: incompatible artifact")
	// ErrIntegrity marks a digest or identity mismatch: a header or section
	// digest, or a declared base ID the records do not reproduce.
	ErrIntegrity = errors.New("prepared: integrity check failed")
	// ErrUnaligned is returned when a View's buffer does not start at a
	// 32-byte aligned address.
	ErrUnaligned = errors.New("prepared: buffer start is not 32-byte aligned (allocate with AlignedBuffer or read with ReadFile)")
)

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrMalformed}, args...)...)
}

func incompatible(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrIncompatible}, args...)...)
}

func integrity(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrIntegrity}, args...)...)
}

// listNameRE is the engine store's list-name rule (engine/store.go
// listNameRE, which is unexported). The builder tests pin the two against
// each other.
var listNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidListName reports whether name passes the store's list-name rule.
func ValidListName(name string) bool { return listNameRE.MatchString(name) }

// hostLittleEndian reports whether this process stores integers
// little-endian. Views reinterpret file bytes as native integers, so a
// big-endian host cannot read an artifact.
var hostLittleEndian = func() bool {
	x := uint16(1)
	return *(*byte)(unsafe.Pointer(&x)) == 1
}()

func checkHost() error {
	if !hostLittleEndian {
		return incompatible("big-endian hosts cannot read prepared artifacts (the format is little-endian and read in place)")
	}
	return nil
}

// AlignedBuffer returns a zeroed n-byte slice whose first byte is 32-byte
// aligned, with capacity n. The Go heap does not move objects, so the
// alignment holds for the slice's lifetime.
func AlignedBuffer(n int) []byte {
	if n < 0 {
		panic("prepared: negative buffer size")
	}
	b := make([]byte, n+Align)
	off := (Align - int(uintptr(unsafe.Pointer(unsafe.SliceData(b)))%Align)) % Align
	return b[off : off+n : off+n]
}

func isAligned(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	return uintptr(unsafe.Pointer(unsafe.SliceData(b)))%Align == 0
}

func align32(n uint64) (uint64, bool) {
	if n > math.MaxUint64-(Align-1) {
		return 0, false
	}
	return (n + Align - 1) &^ (Align - 1), true
}

func le64(b []byte, off int) uint64 { return binary.LittleEndian.Uint64(b[off:]) }
func le32(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }

// section is one decoded table entry.
type section struct {
	kind   uint32
	flags  uint32
	off    uint64
	length uint64
	sum    [digestSize]byte
}

func (s section) end() uint64 { return s.off + s.length } // overflow-checked in parseTable

// table is the decoded header and section table.
type table struct {
	major, minor uint16
	sections     []section
	headEnd      uint64 // end of header + table + header digest
	digest       [digestSize]byte
}

func (t *table) find(kind uint32) (section, bool) {
	for _, s := range t.sections {
		if s.kind == kind {
			return s, true
		}
	}
	return section{}, false
}

// headLen validates the fixed header and returns the byte length of header,
// table and header digest together.
func headLen(b []byte) (int, error) {
	if len(b) < headerSize {
		return 0, malformed("truncated header: %d bytes, need %d", len(b), headerSize)
	}
	if string(b[:8]) != Magic {
		return 0, malformed("bad magic %q (want %q)", b[:8], Magic)
	}
	major := binary.LittleEndian.Uint16(b[8:])
	minor := binary.LittleEndian.Uint16(b[10:])
	if major != MajorVersion {
		return 0, incompatible("format version %d.%d; this build reads %d.x (up to %d.%d)", major, minor, MajorVersion, MajorVersion, MinorVersion)
	}
	if flags := le32(b, 12); flags != 0 {
		return 0, incompatible("format %d.%d: unknown header flags %#x", major, minor, flags)
	}
	n := le32(b, 16)
	if n == 0 || n > MaxSections {
		return 0, malformed("section count %d out of range [1, %d]", n, MaxSections)
	}
	if r := le32(b, 20); r != 0 {
		return 0, malformed("reserved header word is %#x, must be zero", r)
	}
	return headerSize + int(n)*tableEntrySize + digestSize, nil
}

// parseTable decodes and structurally checks the header and section table
// against the file size. head must hold at least headLen bytes.
func parseTable(head []byte, fileSize uint64) (*table, error) {
	hl, err := headLen(head)
	if err != nil {
		return nil, err
	}
	if uint64(hl) > fileSize || len(head) < hl {
		return nil, malformed("truncated section table: file has %d bytes, header and table need %d", fileSize, hl)
	}
	t := &table{
		major:   binary.LittleEndian.Uint16(head[8:]),
		minor:   binary.LittleEndian.Uint16(head[10:]),
		headEnd: uint64(hl),
	}
	copy(t.digest[:], head[hl-digestSize:hl])
	n := int(le32(head, 16))
	first, _ := align32(uint64(hl))
	var prevEnd uint64
	seen := make(map[uint32]bool, n)
	for i := 0; i < n; i++ {
		e := head[headerSize+i*tableEntrySize:]
		s := section{kind: le32(e, 0), flags: le32(e, 4), off: le64(e, 8), length: le64(e, 16)}
		copy(s.sum[:], e[24:56])
		if r := le64(e, 56); r != 0 {
			return nil, malformed("section %d: reserved word is %#x, must be zero", i, r)
		}
		if s.flags&^flagRequired != 0 {
			return nil, malformed("section %d (kind %d): unknown flags %#x", i, s.kind, s.flags)
		}
		if seen[s.kind] {
			return nil, malformed("section %d: duplicate kind %d", i, s.kind)
		}
		seen[s.kind] = true
		if s.off%Align != 0 {
			return nil, malformed("section %d (kind %d): offset %d is not %d-byte aligned", i, s.kind, s.off, Align)
		}
		if s.length > math.MaxUint64-s.off {
			return nil, malformed("section %d (kind %d): offset %d + length %d overflows", i, s.kind, s.off, s.length)
		}
		if s.end() > fileSize {
			return nil, malformed("section %d (kind %d): ends at %d, past the %d-byte file", i, s.kind, s.end(), fileSize)
		}
		if i == 0 {
			if s.off < first {
				return nil, malformed("section 0 (kind %d): offset %d overlaps the header, which ends at %d", s.kind, s.off, hl)
			}
		} else {
			if s.off <= t.sections[i-1].off {
				return nil, malformed("section %d (kind %d): offset %d does not increase", i, s.kind, s.off)
			}
			if s.off < prevEnd {
				return nil, malformed("section %d (kind %d): offset %d overlaps the previous section, which ends at %d", i, s.kind, s.off, prevEnd)
			}
		}
		prevEnd = s.end()
		t.sections = append(t.sections, s)
	}
	if prevEnd != fileSize {
		return nil, malformed("file is %d bytes but the last section ends at %d (trailing or missing bytes)", fileSize, prevEnd)
	}
	for _, s := range t.sections {
		if !knownKind(s.kind) && s.flags&flagRequired != 0 {
			return nil, incompatible("format %d.%d: unknown required section kind %d; this build reads %d.x (up to %d.%d)", t.major, t.minor, s.kind, MajorVersion, MajorVersion, MinorVersion)
		}
	}
	for _, k := range []uint32{KindConfig, KindBuild, KindRecords, KindIDs} {
		if !seen[k] {
			return nil, malformed("missing required section kind %d", k)
		}
	}
	if seen[KindNgram] == seen[KindExact] {
		return nil, malformed("an artifact holds exactly one index section (ngram or exact)")
	}
	if seen[KindExactCompanion] && !seen[KindNgram] {
		return nil, malformed("an exact companion section (kind %d) requires an ngram index section", KindExactCompanion)
	}
	return t, nil
}

func knownKind(k uint32) bool { return k >= KindConfig && k <= KindExactCompanion }

// encodeHead writes header, table and header digest for sections whose
// offsets and digests are already set.
func encodeHead(secs []section) []byte {
	hl := headerSize + len(secs)*tableEntrySize + digestSize
	b := make([]byte, hl)
	copy(b, Magic)
	binary.LittleEndian.PutUint16(b[8:], MajorVersion)
	binary.LittleEndian.PutUint16(b[10:], MinorVersion)
	binary.LittleEndian.PutUint32(b[16:], uint32(len(secs)))
	for i, s := range secs {
		e := b[headerSize+i*tableEntrySize:]
		binary.LittleEndian.PutUint32(e[0:], s.kind)
		binary.LittleEndian.PutUint32(e[4:], s.flags)
		binary.LittleEndian.PutUint64(e[8:], s.off)
		binary.LittleEndian.PutUint64(e[16:], s.length)
		copy(e[24:56], s.sum[:])
	}
	sum := sha256.Sum256(b[:hl-digestSize])
	copy(b[hl-digestSize:], sum[:])
	return b
}
