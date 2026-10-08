package prepared

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"unsafe"
)

// ErrMappedInUse is returned (wrapped) by OpenMapped when this process
// already serves a mapped artifact and the requested file is not that
// artifact, or is that file but no longer carries the identity it was
// admitted with, or the request cannot be served from the admission (a
// LevelStructure request against a trusted admission).
var ErrMappedInUse = errors.New("prepared: this process already serves a mapped artifact")

// ErrNotMappable is returned (wrapped) by OpenMapped, while no mapping is
// admitted, for a file the mapped loader cannot admit because of an
// operating precondition checked on the opened descriptor: it is not a
// regular file, or it has a write permission bit set; and for an attempt
// to admit at LevelStructure. A zero-length file is ErrMalformed instead.
var ErrNotMappable = errors.New("prepared: the mapped loader cannot admit this file")

// OpenMapped maps the artifact at path read-only and verifies the mapped
// bytes themselves at level (LevelFull, LevelTrusted or LevelStructure),
// returning the View over the mapping and what verification established.
// At most one mapped artifact is served per process, for the whole process
// lifetime: nothing is ever unmapped once admitted.
//
// accept, if not nil, is the caller's own acceptance of the verified
// artifact (the engine builds its list in it). It runs after verification
// and before anything is admitted or upgraded; if it returns an error,
// nothing is admitted or published, and that error is returned unchanged
// (unwrapped). On a first admission the never-admitted mapping is then
// unmapped; on a reuse or an upgrade the existing admission is left
// exactly as it was and nothing is unmapped. accept must not retain v, or
// anything derived from it, when it returns an error. accept must not call
// OpenMapped: on a first admission, and on every open while the admission
// is trusted, it runs with the registry lock held.
//
// # Identity on the mapped descriptor
//
// The file is opened once. That descriptor is fstat'ed and must be a
// regular file with no write permission bit at all (mode&0o222 == 0); a
// zero-length file is malformed. Its device and inode are recorded, and
// that same descriptor is mapped with PROT_READ and MAP_SHARED for the
// length the fstat gave. Verification runs on the mapped bytes, never on a
// separate read of the path. No check is made on the pathname alone. The
// descriptor is closed before OpenMapped returns; the mapping outlives it.
//
// # Admission
//
// With nothing admitted:
//   - LevelFull: full verification of the mapped bytes, then accept, then
//     admission as full, recording the device, inode, size, header and
//     table digest, and container SHA-256.
//   - LevelTrusted: trusted verification of the mapped bytes, then accept,
//     then admission as trusted, recording the device, inode, size and
//     header and table digest, and no container digest.
//   - LevelStructure: refused with ErrNotMappable (it establishes no
//     identity to admit).
//
// # Reuse
//
// Every reuse requires, on the new open's own descriptor, the admitted
// device, inode and size, a regular file with no write bits, and then a
// header and table digest of the existing mapping equal to the admitted
// one. Otherwise, and whenever the requested verification of the existing
// mapping fails, the open is refused with ErrMappedInUse and the admission
// is left exactly as it was. By request and admitted policy:
//   - Full against full: full re-verification of the existing mapping; its
//     container SHA-256 and header digest must equal the admitted ones.
//   - Trusted against full: trusted verification of the existing mapping
//     and an equal header digest; served and reported as trusted, never as
//     full.
//   - Structure against full: as full against full; served and reported
//     as LevelFull (a reuse against a full admission always re-verifies at
//     LevelFull, so the engine's Prepared().Level reports LevelFull even
//     though LevelStructure was requested).
//   - Trusted against trusted: trusted verification and an equal header
//     digest; served as trusted, with no hidden full scan.
//   - Full against trusted: real full verification of the existing mapping,
//     which must pass with a header digest equal to the admitted one; then
//     accept; then the admission is upgraded to full by publishing a new
//     immutable record with the container SHA-256 just computed. A failed
//     verification, or a passing one with a different header digest, is
//     ErrMappedInUse: the bytes are not served and nothing is upgraded. A
//     trusted admission never satisfies a full request by itself.
//   - Structure against trusted: ErrMappedInUse, with no verification and
//     no upgrade.
//
// # Concurrency
//
// Admission records are immutable; an upgrade replaces the record under
// the registry lock, so Verified values already returned (which are the
// caller's own copies) never change. The first admission holds the lock
// throughout, so concurrent first opens are serialized and exactly one
// admits. While the admission is trusted, every reuse or upgrade holds the
// lock for its whole transaction (identity checks, verification, accept,
// publication), so requests are handled in lock order against whichever
// record is current. Against a full admission, reuse runs without the lock.
//
// While nothing is admitted, refusals of the file itself (not a regular
// file, a write permission bit set) and a LevelStructure admission attempt
// wrap ErrNotMappable and name the condition; a zero-length file wraps
// ErrMalformed. Once a mapping is admitted, every refusal of the file or
// its identity wraps ErrMappedInUse, and an accept error is returned as is.
//
// # Residual risk
//
// Every safety statement about a mapping holds only while the backing file
// is not modified after admission; a successful open is not continuing
// protection against external mutation. The file must not be modified in
// place for the serving lifetime. That is an operating condition: violating
// it can change answers until restart, because under MAP_SHARED an
// in-place change of the mapped inode is visible through the mapping, and
// stores already open keep serving the changed bytes. Trusted reuse and
// trusted admission cannot see an in-place change to the records, ids or
// index section bytes, at the same inode and size, that stays structurally
// valid and leaves the table's recorded digests unchanged; a full reuse
// sees it. Device, inode, size, mtime and mode are not content integrity.
// Truncating a mapped file can raise SIGBUS on access, crashing the
// process. Permissions (0444) and the macOS user-immutable flag protect
// compliant operation only: any operation able to change the mode or the
// flags, or running as root, can modify the file in place. The
// no-write-bit refusal is a precondition, not proof of immutability.
// Updates use a new artifact file and controlled process turnover.
//
// On platforms without the unix build tag OpenMapped returns an error
// wrapping errors.ErrUnsupported.
func OpenMapped(path string, level Level, accept func(v *View, ver *Verified) error) (*View, *Verified, error) {
	v, ver, _, err := procMapped.open(path, level, accept)
	return v, ver, err
}

// procMapped is the process's one mapped-artifact registry.
var procMapped mappedRegistry

// mappedRegistry admits at most one mapping and never releases it.
type mappedRegistry struct {
	mu  sync.Mutex
	adm *admission // nil until the first admission; replaced only by an upgrade; guarded by mu
}

// admission is an admitted mapping and the identity it was admitted with.
// It is immutable once published: an upgrade publishes a new record.
type admission struct {
	buf      []byte // the mapping, never unmapped
	level    Level  // LevelFull or LevelTrusted
	dev, ino uint64
	size     int64
	header   [digestSize]byte // verified header and table digest (both levels)
	sum      string           // verified container SHA-256 (LevelFull only)
	path     string           // as first opened, for messages
}

// outcome says what an open did (for tests).
type outcome int

const (
	outRefused  outcome = iota
	outAdmitted         // first admission
	outReused           // served from the admission, which is unchanged
	outUpgraded         // served as full, admission upgraded from trusted
)

// fileID is what the new open's own descriptor says.
type fileID struct {
	dev, ino uint64
	mode     os.FileMode
	size     int64
}

// open is OpenMapped, also reporting what it did.
func (r *mappedRegistry) open(path string, level Level, accept func(*View, *Verified) error) (*View, *Verified, outcome, error) {
	if !mmapSupported {
		return nil, nil, outRefused, fmt.Errorf("prepared: mapped loader: memory mapping is not supported on this platform: %w", errors.ErrUnsupported)
	}
	if level != LevelFull && level != LevelStructure && level != LevelTrusted {
		return nil, nil, outRefused, fmt.Errorf("prepared: unknown verification level %d", level)
	}
	if err := checkHost(); err != nil {
		return nil, nil, outRefused, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, outRefused, err
	}
	defer f.Close()
	fi, err := f.Stat() // fstat of this descriptor
	if err != nil {
		return nil, nil, outRefused, err
	}
	dev, ino, err := fileKey(fi)
	if err != nil {
		return nil, nil, outRefused, err
	}
	id := fileID{dev: dev, ino: ino, mode: fi.Mode(), size: fi.Size()}

	r.mu.Lock()
	adm := r.adm
	if adm != nil && adm.level == LevelFull {
		// Full admission: the fast path, without the lock (the record is
		// immutable and a full record is never replaced).
		r.mu.Unlock()
		v, ver, err := adm.reuse(path, id, level)
		if err != nil {
			return nil, nil, outRefused, err
		}
		if accept != nil {
			if err := accept(v, ver); err != nil {
				return nil, nil, outRefused, err
			}
		}
		return v, ver, outReused, nil
	}
	defer r.mu.Unlock()

	if adm != nil {
		// Trusted admission: the whole transaction under the lock.
		v, ver, err := adm.reuse(path, id, level)
		if err != nil {
			return nil, nil, outRefused, err
		}
		if accept != nil {
			if err := accept(v, ver); err != nil {
				return nil, nil, outRefused, err // nothing published
			}
		}
		if level == LevelFull {
			up := *adm
			up.level = LevelFull
			up.sum = ver.ContainerSHA256
			r.adm = &up // a new record; the old one is never mutated
			return v, ver, outUpgraded, nil
		}
		return v, ver, outReused, nil
	}

	// Admission. The file's own preconditions come first, then the level.
	if !id.mode.IsRegular() {
		return nil, nil, outRefused, fmt.Errorf("%w: %s is not a regular file (mode %v)", ErrNotMappable, path, id.mode)
	}
	if id.mode.Perm()&0o222 != 0 {
		return nil, nil, outRefused, fmt.Errorf("%w: %s has write permission bits set (mode %v); a mapped artifact must have none (for example mode 0444) because it must not be modified in place while it is served", ErrNotMappable, path, id.mode.Perm())
	}
	if id.size == 0 {
		return nil, nil, outRefused, malformed("%s is empty", path)
	}
	if id.size < 0 || id.size > math.MaxInt {
		return nil, nil, outRefused, fmt.Errorf("prepared: mapped loader: %s: size %d out of range", path, id.size)
	}
	if level == LevelStructure {
		return nil, nil, outRefused, fmt.Errorf("%w: %s: admitting a mapped artifact requires full or trusted verification; LevelStructure is served only from an already admitted full mapping", ErrNotMappable, path)
	}
	buf, err := mapFile(f, int(id.size))
	if err != nil {
		return nil, nil, outRefused, fmt.Errorf("prepared: mapped loader: mapping %s: %w", path, err)
	}
	if len(buf) != int(id.size) || !isAligned(buf) {
		_ = unmapFile(buf)
		return nil, nil, outRefused, fmt.Errorf("prepared: mapped loader: %s: mapping of %d bytes at %p: %w", path, len(buf), unsafe.SliceData(buf), ErrUnaligned)
	}
	v, ver, err := VerifyBytes(buf, level)
	if err != nil {
		// Never exposed: VerifyBytes returns no View with an error (any
		// View it built internally is unreachable), and nothing else has
		// seen buf.
		_ = unmapFile(buf)
		return nil, nil, outRefused, fmt.Errorf("prepared: mapped loader: %s: %w", path, err)
	}
	if accept != nil {
		if err := accept(v, ver); err != nil {
			// Not admitted, so never exposed: accept must not retain v
			// when it fails, and v is not returned.
			_ = unmapFile(buf)
			return nil, nil, outRefused, err
		}
	}
	r.adm = &admission{buf: buf, level: level, dev: id.dev, ino: id.ino, size: id.size, header: v.tab.digest, sum: ver.ContainerSHA256, path: path}
	return v, ver, outAdmitted, nil
}

// reuse checks that the new open is of the admitted file with the admitted
// identity, verifies the existing mapping at the level the request and
// the admitted policy call for, and returns a new View over it. It never
// changes the admission; every refusal wraps ErrMappedInUse.
func (a *admission) reuse(path string, id fileID, level Level) (*View, *Verified, error) {
	if id.dev != a.dev || id.ino != a.ino {
		return nil, nil, fmt.Errorf("%w: %s (device %d, inode %d) is not the admitted artifact %s (device %d, inode %d), and a mapping is never replaced", ErrMappedInUse, path, id.dev, id.ino, a.path, a.dev, a.ino)
	}
	changed := func(format string, args ...any) error {
		return fmt.Errorf("%w: the admitted file %s changed: "+format, append([]any{ErrMappedInUse, path}, args...)...)
	}
	if !id.mode.IsRegular() {
		return nil, nil, changed("it is no longer a regular file (mode %v)", id.mode)
	}
	if id.mode.Perm()&0o222 != 0 {
		return nil, nil, changed("it has write permission bits set (mode %v)", id.mode.Perm())
	}
	// Checked before the mapping is read: pages past a shrunken end would
	// fault.
	if id.size != a.size {
		return nil, nil, changed("size %d, admitted with %d", id.size, a.size)
	}
	vlevel := level
	if level == LevelStructure {
		if a.level != LevelFull {
			return nil, nil, fmt.Errorf("%w: %s: a LevelStructure open cannot be served from a trusted admission (it is neither verified nor upgraded)", ErrMappedInUse, path)
		}
		vlevel = LevelFull // structure against full: as full against full
	}
	v, ver, err := VerifyBytes(a.buf, vlevel)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: the admitted file %s changed: its mapping no longer verifies at %v: %w", ErrMappedInUse, path, vlevel, err)
	}
	if v.tab.digest != a.header {
		return nil, nil, changed("header and table digest %x, admitted with %x", v.tab.digest, a.header)
	}
	if vlevel == LevelFull && a.level == LevelFull && ver.ContainerSHA256 != a.sum {
		return nil, nil, changed("container SHA-256 %s, admitted with %s", ver.ContainerSHA256, a.sum)
	}
	return v, ver, nil
}
