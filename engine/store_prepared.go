package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/kurn-dev/kurn/engine/prepared"
)

// Loader selects how OpenPrepared brings an artifact into memory.
type Loader int

const (
	// LoaderHeap (the default) reads the whole artifact through one
	// descriptor into one 32-byte aligned heap buffer and serves the list
	// from views over it. The file is not needed after the open returns:
	// replacing or deleting it changes nothing that is served.
	LoaderHeap Loader = iota
	// LoaderMapped serves the list from a read-only, shared memory
	// mapping of the file (prepared.OpenMapped), built into exactly the
	// same list as LoaderHeap: only where the bytes live differs. It must
	// be asked for explicitly. The file must be a regular file with no
	// write permission bit; its mapping is verified in place, and a
	// process serves at most one mapped artifact, retained (never
	// unmapped) for the process lifetime: a later mapped open is served
	// from that mapping only while the file keeps the admitted identity,
	// and is refused with ErrMappedInUse otherwise. The file must not be
	// modified in place while it is served (see OpenPrepared). Without the
	// unix build tag, OpenPrepared refuses it with an error wrapping
	// errors.ErrUnsupported.
	LoaderMapped
)

func (l Loader) String() string {
	switch l {
	case LoaderHeap:
		return "heap"
	case LoaderMapped:
		return "mapped"
	}
	return fmt.Sprintf("Loader(%d)", int(l))
}

// PreparedOptions configure OpenPrepared. The zero value is the serving
// configuration: heap loader, full verification.
type PreparedOptions struct {
	Loader Loader         // zero value: LoaderHeap
	Verify prepared.Level // zero value: prepared.LevelFull
}

// PreparedInfo is what opening a prepared artifact established: the
// artifact verification result, plus what the engine established on top of
// it.
type PreparedInfo struct {
	prepared.Verified

	// IdentitiesRecomputed is true when the engine decoded the artifact's
	// list configuration itself, recomputed the configuration digest and
	// the analyzer digest from it, and found both equal to the declared
	// values. OpenPrepared refuses any artifact where they differ, at every
	// verification level, so it is true for every store it returns.
	IdentitiesRecomputed bool

	// Loader is the loader that brought the artifact into memory.
	Loader Loader

	// StampBasis names what the list's version stamp is computed from:
	// StampBasisVerified ("verified-base-id") when the base ID was
	// recomputed from the records (LevelFull), StampBasisDeclared
	// ("declared-base-id") when it is the build section's declared value
	// (LevelTrusted and LevelStructure). The stamp itself is computed the
	// same way at every level; its basis is reported beside it, never
	// upgraded.
	StampBasis string
}

// The PreparedInfo.StampBasis values.
const (
	StampBasisVerified = "verified-base-id"
	StampBasisDeclared = "declared-base-id"
)

// ErrReadOnly is returned (wrapped) by every mutation of a store opened
// with OpenPrepared, and of its list.
var ErrReadOnly = errors.New("engine: store is read-only (prepared artifact)")

// ErrMappedInUse is returned (wrapped) by OpenPrepared with LoaderMapped
// when this process already serves a mapped artifact and the requested
// file is a different one, or is the admitted file but no longer carries
// the identity it was admitted with. It is prepared.ErrMappedInUse.
var ErrMappedInUse = prepared.ErrMappedInUse

// ErrNotMappable is returned (wrapped) by OpenPrepared with LoaderMapped,
// while no mapping is admitted, for a file that is not a regular file or
// has a write permission bit set (both checked on the opened descriptor),
// and for an attempt to admit at LevelStructure. It is
// prepared.ErrNotMappable.
var ErrNotMappable = prepared.ErrNotMappable

// OpenPrepared opens the prepared artifact at path as a read-only store
// holding its single list, named by the artifact's configuration.
//
// The list is installed straight from the artifact: no record is parsed as
// JSON and no index is rebuilt. Entry IDs, keys and payloads, and the index
// postings, are views into the loaded bytes; only per-entry bookkeeping
// (the entries slice, the ID map) is allocated. Query results' payloads
// alias the artifact bytes and must be treated as read-only, as for any
// list.
//
// Verification runs at opts.Verify; LevelFull (the default) checks every
// digest and recomputes the base ID from the records. LevelTrusted, the
// operator-trusted serving policy, must be asked for explicitly: it keeps
// every structural check, the header and table digest, the digests of the
// config, build and any unknown optional sections, and a JSON syntax check
// of every non-empty payload, and skips the records, ids and index section
// digests, the base-ID recomputation, the canonical-payload check and the
// container digest (see prepared.LevelTrusted). It cannot detect content
// corruption that stays structurally valid, which LevelFull detects, and
// its embedded digests are not authenticity. There is no fallback: a
// failed open at any level returns its error, and nothing is retried at
// another level. LevelStructure is a diagnostic, must be asked for
// explicitly, and is reported in Prepared().Level, as is LevelTrusted,
// together with HeaderDigestVerified and ContentVerified (true only at
// LevelFull). At every level the engine decodes the configuration
// with its own validation and recomputes the configuration and analyzer
// digests; a mismatch with the declared ones fails with an error wrapping
// prepared.ErrIntegrity. A configuration the engine cannot resolve (it does
// not decode strictly, NewList refuses it, or it is not the exact resolved
// form) fails with an error wrapping prepared.ErrIncompatible: after the
// format's own checks pass, such a configuration is indistinguishable from
// version skew. A failed open returns a nil store.
//
// The list's base ID is the verified base ID (at LevelTrusted and
// LevelStructure, which establish none, the declared one), so its version
// stamp is exactly the one a mutable store built from the same entries and
// configuration reports after a restart with an empty journal.
// Prepared().StampBasis says which: verified-base-id at LevelFull,
// declared-base-id otherwise. Prepared().BaseID and ContainerSHA256 are
// empty below LevelFull; DeclaredBaseID is always set.
//
// The store is read-only: every Store and List mutator returns an error
// wrapping ErrReadOnly (SetBaseID and CommitCompact, which cannot return
// one, do nothing), each refusal is counted in ReadOnlyRefusals, and the
// store never writes to disk. It starts no journal and no background
// work. Close keeps its meaning.
//
// # The mapped loader
//
// With LoaderMapped the list is built from views over a read-only
// MAP_SHARED mapping of the file instead of a heap buffer, through the same
// list-building path, so answers, stamps and every guarantee above are the
// same as LoaderHeap's. The identity checks run on the mapped descriptor
// and the verification on the mapped bytes (see prepared.OpenMapped).
// The engine's own checks (the configuration and identity recomputation
// above, and the list build) run before the mapping is admitted or
// upgraded: an artifact the engine refuses is unmapped and leaves nothing
// admitted, and a refused reuse or upgrade leaves the admission unchanged.
// Admission requires LevelFull or LevelTrusted: a LevelStructure open is
// refused unless it is served from an already admitted full mapping,
// whose reuse always re-verifies at LevelFull, so Prepared().Level reports
// LevelFull even when LevelStructure was requested; against a trusted
// admission it is refused with ErrMappedInUse. A trusted request is served
// as trusted whatever was admitted; a full request against a trusted
// admission runs real full verification and upgrades the admission. The
// full request/admission matrix is prepared.OpenMapped's. The mapping is
// retained for the process
// lifetime: Close does not unmap it, and results obtained before Close
// stay readable. While nothing is admitted, a file with any write
// permission bit, one that is not a regular file, and a LevelStructure
// admission attempt are refused with an error wrapping ErrNotMappable (an
// empty file wraps prepared.ErrMalformed); once a mapping is admitted,
// every refusal wraps ErrMappedInUse.
//
// Residual risk: the file must not be modified in place for the serving
// lifetime. That is an operating condition; violating it can change
// answers until restart. Truncating a mapped file can raise SIGBUS on
// access, crashing the process. Permissions (0444) and the macOS
// user-immutable flag protect compliant operation only: any operation able
// to change the mode or the flags, or running as root, can modify the file
// in place. The no-write-bit refusal is a precondition, not proof of
// immutability. Every mapped safety statement holds only while the backing
// file is not modified after admission.
func OpenPrepared(path string, opts PreparedOptions) (*Store, error) {
	switch opts.Loader {
	case LoaderHeap:
	case LoaderMapped:
		// The registry checks the level, the descriptor and the mapped
		// bytes; the store is built by the same preparedStore as the heap
		// loader's, inside the registry's acceptance step, so an artifact
		// the engine refuses is never admitted or upgraded. The callback
		// never calls OpenMapped (it must not re-enter the registry).
		var st *Store
		_, _, err := prepared.OpenMapped(path, opts.Verify, func(v *prepared.View, ver *prepared.Verified) error {
			s, err := preparedStore(v, ver, LoaderMapped)
			if err != nil {
				return err
			}
			st = s
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("engine: OpenPrepared %s: %w", path, err)
		}
		return st, nil
	default:
		return nil, fmt.Errorf("engine: OpenPrepared: unknown loader %v", opts.Loader)
	}
	switch opts.Verify {
	case prepared.LevelFull, prepared.LevelTrusted, prepared.LevelStructure:
	default:
		return nil, fmt.Errorf("engine: OpenPrepared: unknown verification level %v", opts.Verify)
	}
	buf, err := prepared.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("engine: OpenPrepared: %w", err)
	}
	v, ver, err := prepared.VerifyBytes(buf, opts.Verify)
	if err != nil {
		return nil, fmt.Errorf("engine: OpenPrepared %s: %w", path, err)
	}
	st, err := preparedStore(v, ver, opts.Loader)
	if err != nil {
		return nil, fmt.Errorf("engine: OpenPrepared %s: %w", path, err)
	}
	return st, nil
}

// preparedStore builds the read-only store serving the list a verified
// View describes; both loaders install through it.
func preparedStore(v *prepared.View, ver *prepared.Verified, loader Loader) (*Store, error) {
	st := &Store{lists: map[string]*listState{}, readOnly: true}
	l, err := preparedList(v, ver, &st.roRefusals)
	if err != nil {
		return nil, err
	}
	basis := StampBasisDeclared
	if ver.Level == prepared.LevelFull {
		basis = StampBasisVerified
	}
	st.preparedInfo = &PreparedInfo{Verified: *ver, IdentitiesRecomputed: true, Loader: loader, StampBasis: basis}
	ls := &listState{}
	ls.l.Store(l)
	st.lists[l.name] = ls
	return st, nil
}

// preparedList builds the read-only list a verified View describes.
func preparedList(v *prepared.View, ver *prepared.Verified, refusals *atomic.Uint64) (*List, error) {
	c := v.Config()
	l, err := preparedConfig(c)
	if err != nil {
		return nil, err
	}

	// Entries over the buffer: IDs and keys are string views, payloads are
	// slices, all borrowed. One backing array holds every entry's key
	// headers; each entry's keys are a capacity-capped window of it, so an
	// append through one entry can never reach its neighbour.
	n := v.Len()
	entries := make([]Entry, n)
	allKeys := make([]string, 0, ver.Keys)
	var payloadBytes int64
	for i := range entries {
		e := &entries[i]
		e.ID = v.ID(i)
		start := len(allKeys)
		allKeys = v.AppendKeys(allKeys, i)
		if !v.KeysNull(i) {
			// Non-nil even when empty: [] and null are different lines, and
			// the entry must re-encode to the line the base ID hashed.
			e.Keys = allKeys[start:len(allKeys):len(allKeys)]
		}
		if p := v.Payload(i); p != nil {
			e.Payload = p
			payloadBytes += int64(len(p))
		}
	}

	// The base ID is the one full verification recomputed from the
	// records; LevelTrusted and LevelStructure establish none, so the list
	// carries the declared one (Prepared().Level and StampBasis say which).
	baseID := ver.BaseID
	if ver.Level != prepared.LevelFull {
		baseID = ver.DeclaredBaseID
	}
	build := v.Build()
	bi := &IndexBuildInfo{Entries: n, DroppedKeys: build.DroppedKeys, KeylessEntries: build.KeylessEntries}

	// Install through the path engine.Open takes for a base with a
	// verified index cache: identity first, then the prebuilt index.
	l.SetBaseID(baseID)
	switch l.cfg.Match.Mode {
	case "ngram":
		idx, err := v.NgramIndex()
		if err != nil {
			return nil, err
		}
		got := idx.Cfg()
		if !slices.Equal(got.Grams, l.cfg.Match.Grams) || got.StripSpaces != l.cfg.Match.StripSpaces {
			return nil, fmt.Errorf("%w: ngram index config %+v disagrees with the list config", prepared.ErrMalformed, got)
		}
		if l.cfg.Match.ExactCompanion {
			companion, err := v.ExactCompanionIndex()
			if err != nil {
				return nil, err
			}
			if err := l.ReplaceWithIndexesInfo(entries, idx, companion, bi); err != nil {
				return nil, fmt.Errorf("%w: %v", prepared.ErrMalformed, err)
			}
		} else if err := l.ReplaceWithIndexInfo(entries, idx, bi); err != nil {
			return nil, fmt.Errorf("%w: %v", prepared.ErrMalformed, err)
		}
	case "exact":
		idx, err := v.ExactIndex()
		if err != nil {
			return nil, err
		}
		if err := l.ReplaceWithExactIndexInfo(entries, idx, bi); err != nil {
			return nil, fmt.Errorf("%w: %v", prepared.ErrMalformed, err)
		}
	default:
		return nil, fmt.Errorf("%w: unknown match mode %q", prepared.ErrMalformed, l.cfg.Match.Mode)
	}
	// ReplaceWith*IndexInfo totals payload bytes too. Keep assigning the
	// independently decoded artifact total before publication: assignment
	// preserves the prepared loader's existing accounting without doubling it.
	l.snap.Load().base.payloadBytes = payloadBytes

	l.roRefusals = refusals
	l.readOnly = true
	return l, nil
}

// preparedConfig decodes the artifact's list configuration with the
// engine's own validation and recomputes both declared identities from it.
// A configuration it cannot resolve is ErrIncompatible (see OpenPrepared);
// a declared identity it does not reproduce is ErrIntegrity.
func preparedConfig(c prepared.Config) (*List, error) {
	unresolvable := func(reason string) error {
		return fmt.Errorf("%w: the engine cannot resolve the artifact's list configuration: %s", prepared.ErrIncompatible, reason)
	}
	var cfg ListConfig
	dec := json.NewDecoder(bytes.NewReader(c.ListConfig))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, unresolvable(err.Error())
	}
	if dec.More() {
		return nil, unresolvable("trailing data")
	}
	l, err := NewList(c.Name, cfg)
	if err != nil {
		return nil, unresolvable(err.Error())
	}
	// The stored form must be exactly the resolved one NewList installs:
	// otherwise the declared digests could describe a spelling rather than
	// the configuration that is served.
	raw, err := json.Marshal(l.cfg)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, c.ListConfig) {
		return nil, unresolvable("it is not in the resolved canonical form")
	}
	if got := l.ConfigDigest(); got != c.ConfigDigest {
		return nil, fmt.Errorf("%w: config digest: the artifact declares %s, the engine recomputes %s", prepared.ErrIntegrity, c.ConfigDigest, got)
	}
	if got := AnalyzerSpecDigest(l.an); got != c.AnalyzerDigest {
		return nil, fmt.Errorf("%w: analyzer digest: the artifact declares %s, the engine recomputes %s", prepared.ErrIntegrity, c.AnalyzerDigest, got)
	}
	return l, nil
}

// ReadOnly reports whether st was opened from a prepared artifact.
func (st *Store) ReadOnly() bool { return st.readOnly }

// Prepared returns a copy of what OpenPrepared established, or nil for a
// mutable store.
func (st *Store) Prepared() *PreparedInfo {
	if st.preparedInfo == nil {
		return nil
	}
	info := *st.preparedInfo
	return &info
}

// ReadOnlyRefusals counts the mutations a read-only store and its list
// have refused, and the no-op calls they have ignored. Always zero for a
// mutable store.
func (st *Store) ReadOnlyRefusals() uint64 { return st.roRefusals.Load() }

// refuseReadOnly is the first statement of every exported Store mutator.
func (st *Store) refuseReadOnly(op, name string) error {
	if !st.readOnly {
		return nil
	}
	st.roRefusals.Add(1)
	return fmt.Errorf("store: %s %q: %w", op, name, ErrReadOnly)
}

// refuseReadOnly is the first statement of every exported List mutator
// with an error return.
func (l *List) refuseReadOnly(op string) error {
	if !l.ignoreReadOnly() {
		return nil
	}
	return fmt.Errorf("list %s: %s: %w", l.name, op, ErrReadOnly)
}

// ignoreReadOnly reports whether l is read-only, counting the refusal; the
// List mutators without an error return use it to become no-ops.
func (l *List) ignoreReadOnly() bool {
	if !l.readOnly {
		return false
	}
	if l.roRefusals != nil {
		l.roRefusals.Add(1)
	}
	return true
}
