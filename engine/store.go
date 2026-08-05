package engine

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kurn-dev/kurn/engine/analyzer"
	"github.com/kurn-dev/kurn/engine/artifact"
	"github.com/kurn-dev/kurn/engine/exact"
	"github.com/kurn-dev/kurn/engine/ngram"
)

// Store manages the lists in a data directory. Journal is the source of
// truth between snapshots; indexes are always derivable.
//
// Layout per list: <dir>/<name>/{config.json, base.jsonl, base.idx,
// journal.jsonl}. base.idx holds the serialized base index — KURNIDX1 for
// ngram lists, KURNEXA1 for exact lists — and is a pure cache of base.jsonl:
// any load or validation failure falls back to a full rebuild, so a
// bad/stale/missing artifact can never fail startup or corrupt state.
//
// Durability (read this first): there are TWO tiers, and the guarantee
// depends on the crash kind. Base/config writes are always fsynced (file,
// then containing directory after the rename). Journal appends follow
// JournalFsync — with the default FsyncNone, an acknowledged mutation
// survives a PROCESS crash but can be lost to power loss or a machine
// crash (the OS may not have flushed it); FsyncEvery/FsyncInterval close
// that window. Ordering within a mutation: journal/base files are written
// BEFORE the in-memory list is touched, and an operation is only
// acknowledged (nil error) after both — a crash mid-operation can persist
// an unacknowledged mutation (at-least-once), never silently drop an
// acknowledged one within the chosen tier's guarantee. The base.idx
// artifact is the one exception: being a pure cache it is saved LAST, after
// the in-memory install, and its failure is non-fatal (OnArtifactError). See
// Compact and Replace for the per-step crash analysis. Writes are also
// size-bounded up front: a record the reader would refuse (> maxLine) is
// rejected before anything touches disk, so an acknowledged write can never
// brick a later Open.
// Locking: st.mu guards ONLY the lists map (insert/lookup) and is never held
// across IO. Each list has its own mutation lock (listState.lock) spanning
// journal append + memory apply (+ snapshot file ops for Compact/Replace/
// CreateList), so per-list journal order always matches memory order, while
// a multi-second Compact of one list never delays lookups (Store.List/Lists)
// or mutations of any other list. Cross-list ordering is irrelevant: journal
// files are per-list. Queries were always lock-free (List snapshots).
type Store struct {
	dir   string
	mu    sync.RWMutex // guards the lists map and closed; never held across IO
	lists map[string]*listState

	closed    bool           // set by Close: no further mutations, no new work
	closeDone chan struct{}  // closed when the active Close finishes draining
	ops       sync.WaitGroup // mutations and background compactions in flight

	// OnArtifactError, if set, observes non-fatal base.idx save failures
	// (see Compact/Replace: the artifact is a pure cache, so a failed save
	// still acknowledges the operation). Set it before concurrent use.
	//
	// It is called while the list's lock is held, so the callback must not
	// mutate that list (or any list, since it cannot tell which locks the
	// caller holds): doing so self-deadlocks. Log it, count it, or send it
	// to a channel — do not act on it inline.
	OnArtifactError func(list string, err error)

	// Quarantined lists the journals Open could not apply (see openList): a
	// journal that cannot be built (e.g. an exact-mode hot key written
	// before build-time validation existed) is renamed aside and the list
	// opens at its base state — startup never blocks on a bad journal, but
	// the journaled operations are NOT live until the file is repaired by
	// hand, so callers (kurnd) must surface this report. Empty when every
	// journal replayed cleanly.
	Quarantined []JournalQuarantine

	// JournalFsync governs journal-append durability (set before concurrent
	// use; base/config writes are always fsynced regardless):
	//   FsyncNone (default) — no fsync: an acknowledged append survives a
	//     process crash but can be lost to power loss / machine crash.
	//   FsyncEvery — fsync before every acknowledgment: full durability,
	//     one disk flush per mutation.
	//   FsyncInterval — group commit: acknowledgments wait for a shared
	//     flush that runs at most every JournalFsyncInterval, so concurrent
	//     mutations amortize one fsync. Max added latency = the interval.
	JournalFsync FsyncMode

	// JournalFsyncInterval is the FsyncInterval group-commit window
	// (default 2ms when zero).
	JournalFsyncInterval time.Duration

	fsyncOnce    sync.Once     // starts the group-commit goroutine lazily
	fsyncCh      chan fsyncReq // requests to the committer (FsyncInterval only)
	fsyncStop    chan struct{} // closed by Close: the committer must exit
	fsyncStopped chan struct{} // closed by the committer as it returns

	// Skipped lists the list dirs Open could not serve (interrupted
	// create/replace marker, missing/corrupt config, unreadable base — or a
	// stray subdir external tooling dropped in the data dir). The store
	// opens without them: one bad dir must never block every other list.
	// A PUT-replace of the name repairs a broken list dir. Callers (kurnd)
	// must surface this report. Empty when every dir opened cleanly.
	Skipped []ListSkip
}

// JournalQuarantine records one poisoned journal set aside during Open.
type JournalQuarantine struct {
	List string // list name
	Path string // where the unbuildable journal was moved (preserved for repair)
	Err  error  // why it could not be applied
}

// ListSkip records one list dir Open skipped instead of serving.
type ListSkip struct {
	List string // directory name (the would-be list name)
	Err  error  // why it could not be opened
}

// ConfigError marks a CreateList failure caused by the CALLER's input (list
// name or config) as opposed to a store IO fault — servers map the former to
// 400 and the latter to 500. Match with errors.As.
type ConfigError struct{ Err error }

func (e *ConfigError) Error() string { return e.Err.Error() }
func (e *ConfigError) Unwrap() error { return e.Err }

// FsyncMode selects the journal-append durability policy (see
// Store.JournalFsync).
type FsyncMode string

const (
	FsyncNone     FsyncMode = ""         // default: no journal fsync
	FsyncEvery    FsyncMode = "every"    // fsync each append before acking
	FsyncInterval FsyncMode = "interval" // group commit at JournalFsyncInterval
)

// fsyncReq is one append waiting for the group commit to flush its file.
type fsyncReq struct {
	path string
	done chan error
}

// groupCommit blocks until the committer has fsynced path (group commit):
// the acknowledgment that follows is then durable. The committer goroutine
// is started lazily and runs until Store.Close stops it — every request
// (including this blocking wait) happens inside a beginOp/endOp slot, so
// once Close's drain returns no request can be in flight or ever arrive,
// and the committer can exit without stranding a waiter.
func (st *Store) groupCommit(path string) error {
	st.fsyncOnce.Do(func() {
		st.fsyncCh = make(chan fsyncReq)
		st.fsyncStop = make(chan struct{})
		st.fsyncStopped = make(chan struct{})
		interval := st.JournalFsyncInterval
		if interval <= 0 {
			interval = 2 * time.Millisecond
		}
		go fsyncCommitter(st.fsyncCh, st.fsyncStop, st.fsyncStopped, interval)
	})
	req := fsyncReq{path: path, done: make(chan error, 1)}
	st.fsyncCh <- req
	return <-req.done
}

// fsyncCommitter batches fsync requests: the first pending request arms a
// timer; when it fires, every distinct file is fsynced once and all waiters
// are released. fsync flushes the FILE (inode), not a particular fd, so
// opening a fresh fd here durably covers the writer's already-closed one.
// On stop it flushes anything pending (defensively — Close only stops the
// committer after the operation drain, when pending must be empty), closes
// stopped, and returns; before the stop signal existed every retired
// interval-mode store leaked one committer goroutine for the process
// lifetime.
func fsyncCommitter(ch chan fsyncReq, stop <-chan struct{}, stopped chan<- struct{}, interval time.Duration) {
	defer close(stopped)
	pending := map[string][]chan error{}
	flush := func() {
		for path, waiters := range pending {
			err := syncPath(path)
			for _, done := range waiters {
				done <- err
			}
		}
		pending = map[string][]chan error{}
	}
	var timer *time.Timer
	var fire <-chan time.Time
	for {
		select {
		case req := <-ch:
			pending[req.path] = append(pending[req.path], req.done)
			if timer == nil {
				timer = time.NewTimer(interval)
				fire = timer.C
			}
		case <-fire:
			flush()
			timer = nil
			fire = nil
		case <-stop:
			if timer != nil {
				timer.Stop()
			}
			flush()
			return
		}
	}
}

// syncPath fsyncs the file at path via a fresh fd.
func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// syncMarkerRemove makes the .creating marker's removal power-loss-durable
// (a directory fsync); a seam for the same reason as syncJournalTruncate.
var syncMarkerRemove = syncDir

// syncJournalTruncate makes a journal truncation power-loss-durable; a seam
// so tests can pin WHEN it happens (after the truncate, before the in-memory
// install acknowledges) — real power loss is not reproducible in a test.
// Production is syncPath.
var syncJournalTruncate = syncPath

// syncDir fsyncs a directory, making a rename inside it power-loss-durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// listState pairs a list with its mutation lock. l is an atomic pointer so
// readers never touch lock: Store.List during another goroutine's Compact of
// the SAME list must not block either (queries run on snapshots). A nil l is
// the placeholder left by a CreateList that failed before installing the
// list; every reader treats it as absent.
type listState struct {
	lock sync.Mutex // serializes this list's mutations: journal append + memory apply + snapshot file ops
	l    atomic.Pointer[List]

	// autoCompacting dedupes background auto-compaction: at most one
	// in-flight fold per list (see maybeAutoCompact).
	autoCompacting atomic.Bool

	// damage records whether — and HOW — this process
	// lost the ability to vouch that the list's disk state matches what it
	// serves and hashes. The KIND decides which repairs count: content
	// damage is repaired by anything that rebuilds base+journal wholesale,
	// while create damage — a failed PUT window that may have installed a
	// DIFFERENT config.json and always leaves the .creating marker behind
	// — is repaired only by a successful re-create. Replace and Compact
	// rewrite base and journal but neither restores config.json nor
	// removes the marker, so letting them clear create damage produced a
	// "repaired" list whose memory and restart interpreted the same base
	// under different configurations. Append-path mutations are refused
	// while any damage stands; queries keep serving the last acknowledged
	// snapshot. Writers hold lock (ordering); the field itself is atomic
	// so STATUS readers (readiness) never block behind a mutation lock a
	// long compact may hold for seconds.
	damage atomic.Uint32 // holds a damageKind
}

// damageKind classifies why a list can no longer accept appends — see
// listState.damage for the repair semantics each kind implies.
type damageKind uint8

const (
	damageNone damageKind = iota
	// damageContent: base/journal bytes are unverifiable (failed append
	// rollback, or a Replace/Compact that failed after publishing its new
	// base) while config.json still matches the served List. Repaired by
	// any successful destructive rebuild: Replace, Compact, re-create, or
	// reload.
	damageContent
	// damageCreate: a PUT-recreate failed inside its destructive window.
	// The directory may hold the NEW declaration's config.json while
	// memory serves the OLD list, and the fsynced .creating marker
	// survives, so a restart skips the directory entirely. Only a
	// successful re-create repairs; Replace/Compact must refuse rather
	// than acknowledge a "repair" that leaves config and marker behind.
	damageCreate
)

const (
	configFile  = "config.json"
	baseFile    = "base.jsonl"
	idxFile     = "base.idx"
	journalFile = "journal.jsonl"

	// quarantineSuffix is appended to a journal that Open could not apply
	// (see openList): the file is preserved for manual repair under
	// journal.jsonl+quarantineSuffix, and the list opens at its base state.
	quarantineSuffix = ".quarantined"

	// markerFile brackets CreateList's non-atomic window (data wipe + config
	// write): written before the wipe, removed after the config lands. A
	// surviving marker means a create/replace died mid-window — the dir's
	// contents are indeterminate (old config over wiped data, or no config at
	// all), so Open skips the list instead of serving a half-state.
	markerFile = ".creating"

	// maxLine bounds journal/base lines (matches the planned NDJSON limit).
	maxLine = 1 << 20
)

// listNameRE validates list names, which become directory names.
var listNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// sweepTempFiles removes .base-* / .cfg-* / .idx-* survivors of crashed
// atomic writes (writeBaseTemp / writeFileAtomic / artifact.Save clean up via
// defers, which a crash skips). Every CreateTemp prefix under a list
// directory must appear here: one missed prefix leaks one file per crash,
// forever, since nothing else ever looks at them.
// Best-effort: a failed remove just leaves the orphan for the next Open. Runs
// at Open, when no writer can be mid-flight.
func sweepTempFiles(lp string) {
	for _, pat := range []string{".base-*", ".cfg-*", ".idx-*"} {
		matches, _ := filepath.Glob(filepath.Join(lp, pat))
		for _, m := range matches {
			os.Remove(m)
		}
	}
}

// EntryTooLargeError reports an entry whose serialized record (journal or
// base line, trailing newline included) would exceed maxLine. It is returned
// BEFORE anything touches disk — the write is rejected, nothing persisted —
// so callers can safely treat it as bad input rather than a store failure.
// Match with errors.As.
type EntryTooLargeError struct {
	ID   string // the offending entry's ID
	Size int    // serialized record size in bytes, newline included
}

func (e *EntryTooLargeError) Error() string {
	return fmt.Sprintf("store: entry %q: record is %d bytes, exceeding the %d-byte line limit", e.ID, e.Size, maxLine)
}

// Open loads every list in dir (created if needed): config.json → NewList,
// base.jsonl → base segment (via the base.idx artifact when it loads cleanly
// and its index-time config matches; ANY artifact error falls back to a full
// build — a bad artifact must never fail startup), then the journal is
// replayed in one batch. A list dir that cannot be opened (interrupted
// create marker, missing/corrupt config, unreadable base, stray subdir) is
// skipped and recorded in Skipped — one bad dir never blocks the store.
// Only an unreadable/uncreatable data dir itself is fatal.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	st := &Store{dir: dir, lists: map[string]*listState{}}
	for _, de := range ents {
		if !de.IsDir() {
			continue
		}
		name := de.Name()
		sweepTempFiles(st.listPath(name))
		l, err := st.openList(name)
		if err != nil {
			st.Skipped = append(st.Skipped, ListSkip{List: name, Err: err})
			continue
		}
		ls := &listState{}
		ls.l.Store(l)
		st.lists[name] = ls
	}
	return st, nil
}

// get returns the named list's state for mutation, or an unknown-list error.
// The current *List must be re-loaded from ls.l AFTER acquiring ls.lock: a
// concurrent CreateList may swap in a fresh list first.
func (st *Store) get(name string) (*listState, error) {
	st.mu.RLock()
	ls := st.lists[name]
	st.mu.RUnlock()
	if ls == nil || ls.l.Load() == nil {
		return nil, fmt.Errorf("store: unknown list %q", name)
	}
	return ls, nil
}

func (st *Store) openList(name string) (*List, error) {
	lp := st.listPath(name)
	// Marker check FIRST: a valid-looking old config may sit over wiped data
	// (PUT-replace died mid-window) — serving it would silently revert the
	// list. The dir's contents are indeterminate; only a fresh PUT repairs it.
	if _, err := os.Stat(filepath.Join(lp, markerFile)); err == nil {
		return nil, fmt.Errorf("%s present: create/replace was interrupted mid-window; dir contents are indeterminate — re-PUT the list config to repair", markerFile)
	}
	raw, err := os.ReadFile(filepath.Join(lp, configFile))
	if err != nil {
		return nil, err
	}
	var cfg ListConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", configFile, err)
	}
	l, err := NewList(name, cfg)
	if err != nil {
		return nil, err
	}

	// Base snapshot. A missing base.jsonl means an empty base (fresh list).
	entries, haveBase, baseID, err := readBase(filepath.Join(lp, baseFile))
	if err != nil {
		return nil, err
	}
	if !haveBase {
		baseID = "empty"
	}
	// Identity first: every stamp from here on — base install and journal
	// replay — carries the restart-stable content-addressed form.
	l.SetBaseID(baseID)
	if haveBase {
		if !st.installWithArtifact(l, entries, filepath.Join(lp, idxFile)) {
			if err := l.Replace(entries); err != nil {
				return nil, err
			}
		}
	} else {
		// No base: the virgin snapshot must still stamp the full form, or
		// an empty created list would change stamps across a restart.
		l.stampFresh()
	}

	// Journal replay: one batch, no re-append.
	jp := filepath.Join(lp, journalFile)
	ops, tornAt, err := readJournal(jp)
	if err != nil {
		return nil, err
	}
	if tornAt >= 0 {
		// Repair the torn tail NOW, before any new append: the fragment has
		// no trailing newline, so a later Store.Upsert would append its
		// record onto the same line — an acknowledged write that the next
		// restart would then silently drop. Truncating to the end of the
		// last intact record keeps disk exactly equal to what replay saw.
		if err := os.Truncate(jp, tornAt); err != nil {
			return nil, fmt.Errorf("%s: repairing torn tail: %w", journalFile, err)
		}
	}
	// Seed the running journal content hash from the intact prefix — the
	// bytes replay actually consumes, post any torn-tail truncate. The byte
	// count doubles as the version stamp's replay position, so hash and
	// position are computed from one read and cannot disagree.
	jh := newJournalHash()
	jpos := int64(0)
	if jf, jerr := os.Open(jp); jerr == nil {
		n, cerr := io.Copy(jh, jf)
		jf.Close()
		if cerr != nil {
			return nil, fmt.Errorf("%s: hashing replay prefix: %w", journalFile, cerr)
		}
		jpos = n
	} else if !os.IsNotExist(jerr) {
		return nil, jerr
	}
	l.setJournalHash(jh)
	if len(ops) > 0 {
		l.mu.Lock()
		err := l.applyJournalLocked(ops, jpos)
		l.mu.Unlock()
		if err != nil {
			// Poisoned journal (an exact-mode hot key written before build-
			// time validation existed, or a hand-edited/corrupted file whose
			// records still parse): replay can never succeed, so leaving the
			// file in place would make every future Open fail the same way.
			// Quarantine it aside and open the list at its base state — a bad
			// journal must never block startup. The journaled operations are
			// preserved in the quarantined file for manual repair, and the
			// degradation is recorded in the store's Quarantined report.
			q := jp + quarantineSuffix
			os.Remove(q) // single quarantine slot: a previous one is stale forensics
			if rerr := os.Rename(jp, q); rerr != nil {
				return nil, fmt.Errorf("%s: unbuildable journal (%v) and quarantine rename failed: %w", journalFile, err, rerr)
			}
			// The journal file is gone: the list serves its base state and
			// future appends start an empty journal, so the running hash
			// must forget the quarantined bytes.
			l.setJournalHash(newJournalHash())
			st.mu.Lock() // openList runs at Open (single-threaded) AND ReloadList (concurrent)
			st.Quarantined = append(st.Quarantined, JournalQuarantine{List: name, Path: q, Err: err})
			st.mu.Unlock()
		}
	}
	return l, nil
}

// installWithArtifact tries the fast path: load base.idx and install it via
// ReplaceWithIndex (ngram) or ReplaceWithExactIndex (exact). Returns false —
// caller must do a full build — on ANY failure: missing/corrupt artifact,
// wrong artifact kind for the mode (the magics differ), index-time config
// mismatch (grams or strip_spaces changed in config.json since the artifact
// was saved), ANALYZER digest mismatch (the list's analyzer spec changed —
// e.g. config.json hand-edited between restarts — so the artifact's keys are
// normalized differently than queries would be; for exact indexes, which
// have no other index-time knobs, the digest is the whole identity check),
// a pre-digest artifact or one without build info (both unknown ⇒ rebuild
// once, which is also what makes the restored loss counters trustworthy),
// or entries/ordinal mismatch. Takes ownership of entries only on success.
func (st *Store) installWithArtifact(l *List, entries []Entry, idxPath string) bool {
	wantDigest := AnalyzerSpecDigest(l.an)
	// The identity check is the one that carries correctness. An index maps
	// grams to ordinal POSITIONS in one specific base.jsonl; against any
	// other content — even of identical length — every structural check can
	// pass while queries attribute one entry's matching evidence to
	// another (entity C returned with entity A's score and key). The
	// analyzer digest, config, and counts cannot see that; only the base
	// content hash can, so an artifact recording a different or absent
	// identity rebuilds.
	wantBase := l.BaseIDForArtifact()
	if l.cfg.Match.Mode == "exact" {
		idx, digest, build, err := artifact.LoadExact(idxPath)
		if err != nil || digest == "" || digest != wantDigest || build == nil || build.BaseID != wantBase {
			return false
		}
		return l.ReplaceWithExactIndexInfo(entries, idx, buildInfo(build)) == nil
	}
	if l.cfg.Match.Mode != "ngram" {
		return false
	}
	idx, digest, build, err := artifact.Load(idxPath)
	if err != nil || digest == "" || digest != wantDigest || build == nil || build.BaseID != wantBase {
		return false
	}
	want := ngram.Config{Grams: l.cfg.Match.Grams, StripSpaces: l.cfg.Match.StripSpaces}
	got := idx.Cfg()
	if !slices.Equal(got.Grams, want.Grams) || got.StripSpaces != want.StripSpaces {
		return false
	}
	return l.ReplaceWithIndexInfo(entries, idx, buildInfo(build)) == nil
}

// CreateList creates (or PUT-replaces) a list: validates the name (it becomes
// a directory) and config, wipes any existing data files, writes the resolved
// config, and installs a fresh empty list in memory. Files first, memory
// second, and the in-memory swap is the LAST statement: every error path —
// including the four after the wipe has begun — returns without swapping, so
// memory keeps serving the old list while disk no longer matches it. That
// divergence is intentional and bounded by the marker: it survives all four,
// so the next Open skips the directory rather than serving the mismatch, and
// the repair is a fresh PUT. The nastiest case is the last one, where disk
// holds a complete and valid new list and only the marker removal failed.
func (st *Store) CreateList(name string, cfg ListConfig) (*List, error) {
	l, _, err := st.CreateListVersioned(name, cfg)
	return l, err
}

// CreateListVersioned is CreateList returning the fresh list's version
// stamp, captured under the creation lock (see UpsertVersioned for why a
// later Version() lookup is not evidence of THIS operation).
func (st *Store) CreateListVersioned(name string, cfg ListConfig) (*List, string, error) {
	if err := st.beginOp(); err != nil {
		return nil, "", err
	}
	defer st.endOp()
	if !listNameRE.MatchString(name) {
		return nil, "", &ConfigError{fmt.Errorf("store: invalid list name %q", name)}
	}
	// Resolve a preset to its explicit step list BEFORE the config is
	// persisted: config.json is the authority on reopen, so a future edit to
	// a built-in preset must never silently re-analyze an existing list
	// differently. The preset name is cleared (ResolveAnalyzer prefers Preset
	// over Steps when both are set); an unknown preset falls through
	// unchanged for NewList to reject with its usual error.
	if steps, ok := analyzer.PresetSteps(cfg.Analyzer.Preset); ok {
		cfg.Analyzer = AnalyzerConfig{Steps: steps}
	}
	l, err := NewList(name, cfg) // validates config, applies defaults
	if err != nil {
		return nil, "", &ConfigError{err}
	}
	st.mu.Lock()
	ls := st.lists[name]
	if ls == nil {
		ls = &listState{} // nil l until the create completes; readers treat it as absent
		st.lists[name] = ls
	}
	st.mu.Unlock()
	ls.lock.Lock() // waits out any in-flight mutation of the list being replaced
	defer ls.lock.Unlock()
	lp := st.listPath(name)
	if err := os.MkdirAll(lp, 0o755); err != nil {
		return nil, "", err
	}
	// Marker brackets the non-atomic wipe+config window: if the process dies
	// anywhere inside it, the surviving marker makes Open skip the dir (its
	// contents are indeterminate) instead of serving a config-less dir or —
	// worse — the OLD config over wiped data (silent revert). Removed only
	// after the new config is durably in place.
	// The marker must reach the disk BEFORE the wipe, not merely the page
	// cache: on power loss the removes can persist while an unsynced marker
	// does not, leaving exactly the old-config-over-wiped-data state the
	// marker exists to prevent. writeFileAtomic fsyncs the file and the
	// directory entry, so once it returns a crash cannot lose the bracket.
	mp := filepath.Join(lp, markerFile)
	if err := writeFileAtomic(mp, []byte("create/replace in progress\n")); err != nil {
		return nil, "", err
	}
	// PUT-replace semantics: prior snapshot/journal are gone. From the
	// first remove onward the operation is destructive and cannot roll
	// back: on ANY error below, old memory keeps serving (correct — the
	// new list was never acknowledged) but it must stop accepting
	// appends, because the marker-bracketed directory no longer matches
	// it — a restart either skips the dir (marker present) or loads the
	// NEW declaration, and either way a post-failure append would be
	// stranded or replayed against state the caller never acknowledged.
	for _, f := range []string{baseFile, idxFile, journalFile} {
		if err := os.Remove(filepath.Join(lp, f)); err != nil && !os.IsNotExist(err) {
			ls.setDamage(damageCreate)
			return nil, "", err
		}
	}
	raw, err := json.Marshal(l.Config()) // resolved config (defaults applied)
	if err != nil {
		ls.setDamage(damageCreate)
		return nil, "", err
	}
	if err := writeFileAtomic(filepath.Join(lp, configFile), append(raw, '\n')); err != nil {
		ls.setDamage(damageCreate)
		return nil, "", err
	}
	if err := os.Remove(mp); err != nil {
		// Disk state is complete but the marker survives: without this remove
		// the next Open would skip the list, so surface the failure now.
		ls.setDamage(damageCreate)
		return nil, "", fmt.Errorf("store: clearing %s: %w", markerFile, err)
	}
	// The removal must be DURABLE before the create is acknowledged: the
	// marker was fsynced into existence, so a power loss after an
	// acknowledged PUT could resurrect the directory entry and make the
	// next Open skip an otherwise complete list as "interrupted".
	if err := syncMarkerRemove(lp); err != nil {
		ls.setDamage(damageCreate)
		return nil, "", fmt.Errorf("store: syncing %s removal: %w", markerFile, err)
	}
	l.SetBaseID("empty") // store-managed: content-addressed stamps from the start
	l.setJournalHash(newJournalHash())
	l.stampFresh()           // the virgin snapshot carries the full form, config digest included
	ls.setDamage(damageNone) // wiped and rebuilt wholesale: any prior damage is repaired
	ls.l.Store(l)
	// Captured under ls.lock: the version this create committed, not
	// whatever a later mutation may have installed by audit time.
	return l, l.snap.Load().version, nil
}

// Upsert journals the entries then applies them to the in-memory list. The
// mutation is PREPARED first (the overlay segment is built — the one
// fallible step — without touching list state), so an unbuildable batch
// (exact-mode hot key) is rejected before anything touches disk: poison can
// never be journaled. Journal precedes commit: if the append fails nothing
// is applied and the error is returned; if the process dies between append
// and commit, restart replay converges memory to the journal (replay
// rebuilds the same overlay deterministically, and it is buildable because
// the prepare succeeded). The per-list lock plus l.mu span
// prepare+append+commit, so journal order always matches memory order.
func (st *Store) Upsert(name string, entries []Entry) error {
	_, err := st.UpsertVersioned(name, entries)
	return err
}

// UpsertVersioned is Upsert additionally returning the committed version
// stamp, captured under the same locks that committed the mutation. An
// audit record pairing this mutation with a version looked up AFTERWARDS
// can be stamped with a LATER mutation's version; this return is the only
// race-free way to know which version an acknowledged mutation produced.
func (st *Store) UpsertVersioned(name string, entries []Entry) (string, error) {
	if err := st.beginOp(); err != nil {
		return "", err
	}
	defer st.endOp()
	ls, err := st.get(name)
	if err != nil {
		return "", err
	}
	ls.lock.Lock()
	defer ls.lock.Unlock()
	return st.upsertLocked(name, ls, ls.l.Load(), entries)
}

// upsertLocked is the shared body of Upsert and UpsertGen; ls.lock is held.
// Returns the committed version (see UpsertVersioned).
func (st *Store) upsertLocked(name string, ls *listState, l *List, entries []Entry) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, err := l.prepareUpsertLocked(entries)
	if err != nil {
		return "", err
	}
	recs := make([]journalRec, len(entries))
	for i := range entries {
		recs[i] = journalRec{Op: "upsert", Entry: &entries[i]}
	}
	jpos, err := st.appendJournal(ls, l, filepath.Join(st.listPath(name), journalFile), recs)
	if err != nil {
		return "", err
	}
	l.commitOverlayLockedAt(b, jpos)
	st.maybeAutoCompact(name, ls, l)
	return l.snap.Load().version, nil
}

// ListReplacedError reports that a list was recreated (PUT) or reloaded
// while a caller held a reference to an earlier generation of it. Streaming
// callers use it to stop rather than let the rest of one upload land in a
// list that was swapped underneath them.
type ListReplacedError struct{ Name string }

func (e *ListReplacedError) Error() string {
	return fmt.Sprintf("store: list %q was replaced while the write was in flight", e.Name)
}

// UpsertGen is Upsert, refused with *ListReplacedError unless the named list
// is still the generation the caller captured. A multi-batch stream resolves
// the list by NAME on every batch, so a concurrent CreateList or ReloadList
// would otherwise silently land the remainder of one upload in a different
// list while the client is told its whole upload succeeded. The check runs
// under the same lock as the write, so the generation cannot change between
// the two.
//
// Whole-content replacement of the SAME generation (Store.Replace) is not a
// generation change and is not refused: concurrent writers to one list are
// last-writer-wins, as they are for any other mutation.
func (st *Store) UpsertGen(name string, gen *List, entries []Entry) error {
	_, err := st.UpsertGenVersioned(name, gen, entries)
	return err
}

// UpsertGenVersioned is UpsertGen returning the committed version stamp
// (see UpsertVersioned). A streaming caller audits with the LAST batch's
// version — the one covering everything it acknowledged.
func (st *Store) UpsertGenVersioned(name string, gen *List, entries []Entry) (string, error) {
	if err := st.beginOp(); err != nil {
		return "", err
	}
	defer st.endOp()
	ls, err := st.get(name)
	if err != nil {
		return "", err
	}
	ls.lock.Lock()
	defer ls.lock.Unlock()
	l := ls.l.Load()
	if gen != nil && l != gen {
		return "", &ListReplacedError{Name: name}
	}
	return st.upsertLocked(name, ls, l, entries)
}

// Delete journals the delete then applies it. Same prepare-first ordering
// rationale as Upsert.
func (st *Store) Delete(name string, id string) error {
	_, err := st.DeleteVersioned(name, id)
	return err
}

// DeleteVersioned is Delete returning the committed version stamp (see
// UpsertVersioned).
func (st *Store) DeleteVersioned(name string, id string) (string, error) {
	if err := st.beginOp(); err != nil {
		return "", err
	}
	defer st.endOp()
	ls, err := st.get(name)
	if err != nil {
		return "", err
	}
	ls.lock.Lock()
	defer ls.lock.Unlock()
	l := ls.l.Load()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, err := l.prepareDeleteLocked(id)
	if err != nil {
		return "", err
	}
	jpos, err := st.appendJournal(ls, l, filepath.Join(st.listPath(name), journalFile), []journalRec{{Op: "delete", ID: id}})
	if err != nil {
		return "", err
	}
	l.commitOverlayLockedAt(b, jpos)
	st.maybeAutoCompact(name, ls, l)
	return l.snap.Load().version, nil
}

// ReloadList re-opens the named list from its on-disk dir — the publish
// path: a prebuilt bundle's files are shipped into the list dir
// (temp-name + rename per file, base.idx first-removed then last-renamed,
// the same discipline persistBase uses), then ReloadList swaps the result
// in. The reopen builds a NEW List and installs it only on success: a
// corrupt or half-shipped bundle leaves the currently-served list exactly
// as it was. A list new to this store (bundle shipped into a fresh dir)
// is added; the name is validated (it became a directory).
func (st *Store) ReloadList(name string) (*List, error) {
	l, _, err := st.ReloadListVersioned(name)
	return l, err
}

// ReloadListVersioned is ReloadList returning the reloaded list's version
// stamp, captured under the reload lock (see UpsertVersioned for why a
// later Version() lookup is not evidence of THIS operation).
func (st *Store) ReloadListVersioned(name string) (*List, string, error) {
	if err := st.beginOp(); err != nil {
		return nil, "", err
	}
	defer st.endOp()
	if !listNameRE.MatchString(name) {
		return nil, "", &ConfigError{fmt.Errorf("store: invalid list name %q", name)}
	}
	st.mu.Lock()
	ls := st.lists[name]
	if ls == nil {
		ls = &listState{} // nil l until the reload succeeds; readers treat as absent
		st.lists[name] = ls
	}
	st.mu.Unlock()
	ls.lock.Lock()
	defer ls.lock.Unlock()
	// Fail-safe (review finding): a bundle replaces ALL list content, so a
	// leftover journal means the ship discipline was violated — replaying
	// it would serve content the bundle's manifest doesn't describe.
	// Refuse rather than replay or silently delete acknowledged mutations.
	if fi, err := os.Stat(filepath.Join(st.listPath(name), journalFile)); err == nil && fi.Size() > 0 {
		return nil, "", fmt.Errorf("store: %s has a non-empty %s — a bundle replaces all content; the shipper must remove the journal (ship discipline, platform contract §5)", name, journalFile)
	}
	sweepTempFiles(st.listPath(name))
	l, err := st.openList(name)
	if err != nil {
		return nil, "", err
	}
	ls.setDamage(damageNone) // openList rebuilt the List from a coherent, marker-free dir
	ls.l.Store(l)
	return l, l.snap.Load().version, nil
}

// maybeAutoCompact schedules a background fold when the list's configured
// overlay threshold is reached. Called at the tail of Upsert/Delete while
// the caller still holds ls.lock — the goroutine blocks on that lock, so
// the fold runs strictly OFF the mutation path (the mutation acks first).
// autoCompacting keeps at most one fold in flight per list; a fold that
// fails (IO) simply leaves the overlay over threshold, so the next mutation
// re-triggers it — no retry loop of its own. The goroutine re-checks the
// threshold before folding: by the time it runs, a manual compact or a
// PUT-replace may already have emptied the overlay.
func (st *Store) maybeAutoCompact(name string, ls *listState, l *List) {
	thr := l.Config().OverlayAutoCompact
	if thr <= 0 {
		return
	}
	if _, ov, _ := l.Stats(); ov < thr {
		return
	}
	if !ls.autoCompacting.CompareAndSwap(false, true) {
		return
	}
	if !st.spawnBG(func() {
		defer ls.autoCompacting.Store(false)
		cur := ls.l.Load()
		if cur == nil {
			return
		}
		if _, ov, _ := cur.Stats(); ov < thr {
			return
		}
		st.Compact(name) // error: overlay stays over threshold; next mutation re-triggers
	}) {
		ls.autoCompacting.Store(false)
	}
}

// Compact folds the overlay into a new base: prepare (memory-invisible),
// persist, then swap. Step order is chosen so every crash point (and every
// error return) recovers to the ACKNOWLEDGED state on restart, and a failed
// step leaves the served list untouched:
//
//  1. l.PrepareCompact() — builds the folded segment without installing it;
//     an error (unbuildable fold) leaves everything as it was.
//  2. write the new base.jsonl CONTENT to a temp file (size-validated; an
//     error here leaves every real file AND memory untouched). The entries
//     come straight from the prepared fold — the live set in ID order.
//  3. remove base.idx — from here until (6) there is no artifact, so restart
//     full-builds from whichever base.jsonl is in place. Removing before (4)
//     is what prevents the OLD artifact from ever sitting next to the NEW
//     base.jsonl, where a config-matching stale index whose NumOrds happens
//     to fit could be installed with wrong ordinal→entry mappings.
//  4. rename the temp file to base.jsonl. A crash between (4) and (5)
//     leaves the new base plus the old journal: restart re-replays the
//     journal over the already-folded base — idempotent for the live set
//     (upsert of an identical entry / delete of an absent one), scores may
//     differ from the never-acknowledged fold, which is fine.
//  5. truncate journal AND fsync the truncation — only after the new base
//     is fully in place, and durably before (6): the new base's rename is
//     already synced, so an unsynced truncate could survive power loss as
//     "new base + old journal" for an ACKNOWLEDGED fold, replaying stale
//     mutations over it. A FAILED truncate or sync returns the error with
//     memory still pre-compact: the served state matches what was
//     acknowledged, disk recovers to the same live set on restart.
//  6. l.CommitCompact — the in-memory swap, stamped with the persisted
//     content hash. Acknowledged from here.
//  7. artifact.Save base.idx — LAST and non-fatal: the artifact is a pure
//     cache of base.jsonl with a mandatory full-rebuild fallback on load, so
//     a failed save must not fail an otherwise-durable compact. The caller
//     still gets nil; OnArtifactError (if set) observes the error.
func (st *Store) Compact(name string) error {
	_, err := st.CompactVersioned(name)
	return err
}

// CompactResult is what one compact COMMITTED: the list generation it
// operated on (resolved under the store lock — a concurrent PUT can swap
// the name to a new generation between a caller's lookup and the compact),
// the committed version, and the folded live-entry count. Callers writing
// audit lines or responses must take all three from here; re-reading them
// through a pointer resolved before the call can describe a different
// generation or a later mutation.
type CompactResult struct {
	List    *List
	Version string
	Entries int
}

// CompactVersioned is Compact returning what it committed (see
// CompactResult and UpsertVersioned).
func (st *Store) CompactVersioned(name string) (CompactResult, error) {
	if err := st.beginOp(); err != nil {
		return CompactResult{}, err
	}
	defer st.endOp()
	ls, err := st.get(name)
	if err != nil {
		return CompactResult{}, err
	}
	ls.lock.Lock()
	defer ls.lock.Unlock()
	// See ReplaceVersioned: create damage needs a re-create, and a compact
	// (including a background auto-compact reaching this path) must not
	// clear it as a side effect.
	if ls.getDamage() == damageCreate {
		return CompactResult{}, ls.damageErr()
	}
	l := ls.l.Load()
	b, err := l.PrepareCompact()
	if err != nil {
		return CompactResult{}, err
	}
	lp := st.listPath(name)
	id, published, err := st.persistBase(lp, b.live)
	if err != nil {
		if published {
			// The folded base is on disk but this operation failed: memory
			// keeps serving the acknowledged pre-compact state, and any
			// further append would write into a journal that a restart
			// replays over the NEVER-acknowledged fold. Refuse mutations
			// until a destructive retry succeeds.
			ls.setDamage(damageContent)
		}
		return CompactResult{}, err
	}
	if err := os.Truncate(filepath.Join(lp, journalFile), 0); err != nil {
		if !os.IsNotExist(err) {
			ls.setDamage(damageContent) // new base published, old journal beside it
			return CompactResult{}, err // a missing journal is already empty
		}
	} else if err := syncJournalTruncate(filepath.Join(lp, journalFile)); err != nil {
		// The truncate must be DURABLE before the fold is acknowledged:
		// power loss can otherwise keep the new base (synced by
		// persistBase) while the journal's shorter length is lost, and
		// restart would replay the pre-compact journal over the folded
		// base. Failing here leaves memory pre-compact — the acknowledged
		// state — and the list damaged: the published fold cannot be
		// rolled back, so writes must wait for a successful retry.
		ls.setDamage(damageContent)
		return CompactResult{}, err
	}
	l.setJournalHash(newJournalHash()) // truncated: the running hash restarts with the file
	ls.setDamage(damageNone)
	l.CommitCompact(b, id)
	st.saveArtifact(l, lp, b.seg.ng, b.seg.ex)
	return CompactResult{List: l, Version: l.snap.Load().version, Entries: len(b.live)}, nil
}

// Replace swaps the whole list content: new base + empty journal on disk,
// then the in-memory swap, then the artifact cache. Entries are deduped
// (last-wins, like List.Replace) ONCE, and the same slice feeds base.jsonl,
// the in-memory segment, and the saved artifact, so all three agree. Step
// order and crash/error points:
//
//  1. prepareBase: dedupe + build the new segment in memory (nothing visible).
//  2. write the new base.jsonl content to a temp file (size-validated; an
//     error leaves every real file untouched — nothing persisted).
//  3. remove base.idx (stale-artifact guard, exactly as in Compact).
//  4. rename the temp file to base.jsonl (commit point). A crash between (4)
//     and (5) leaves the new base plus the OLD journal, so restart replays
//     pre-replace ops onto the new base — an anomaly, but only for a Replace
//     that was never acknowledged; truncating the journal BEFORE (4) instead
//     would lose acknowledged upserts/deletes on a crash between the two.
//  5. truncate journal and fsync the truncation, so the acknowledged
//     replacement cannot power-loss back into "new base + old journal" —
//     which would resurrect the entries the Replace removed.
//  6. install the prebuilt segment in memory — memory now equals disk and the
//     operation is acknowledged from here on.
//  7. artifact.Save base.idx — LAST and non-fatal (pure cache, rebuild
//     fallback): a failed save — or a crash before it — just means the next
//     Open full-builds from base.jsonl. The caller still gets nil;
//     OnArtifactError (if set) observes the error. Ordering the save after
//     (6) means an artifact failure can never leave old memory serving a
//     replaced on-disk list.
func (st *Store) Replace(name string, entries []Entry) error {
	_, err := st.ReplaceVersioned(name, entries)
	return err
}

// ReplaceVersioned is Replace returning the committed version stamp (see
// UpsertVersioned).
func (st *Store) ReplaceVersioned(name string, entries []Entry) (string, error) {
	if err := st.beginOp(); err != nil {
		return "", err
	}
	defer st.endOp()
	ls, err := st.get(name)
	if err != nil {
		return "", err
	}
	ls.lock.Lock()
	defer ls.lock.Unlock()
	// Create damage cannot be repaired here: Replace rewrites base and
	// journal but leaves the failed PUT's config.json and .creating marker
	// in place — acknowledging it as a repair produced a list whose memory
	// and restart interpreted the same base under different configurations
	// (and whose directory a restart skips entirely). Only a re-create
	// rebuilds the whole declaration.
	if ls.getDamage() == damageCreate {
		return "", ls.damageErr()
	}
	l := ls.l.Load()
	entries, seg, err := l.prepareBase(entries)
	if err != nil {
		// Unbuildable base (exact-mode hot key): nothing persisted, nothing
		// installed — the list is exactly as before.
		return "", err
	}
	lp := st.listPath(name)
	id, published, err := st.persistBase(lp, entries)
	if err != nil {
		if published {
			// The replacement base is on disk, memory still serves the old
			// list, and the operation was NOT acknowledged. A later append
			// would be prepared against old memory but land in a journal a
			// restart replays over the new base — recovering to a state
			// nobody acknowledged. Contain it: refuse mutations until a
			// destructive retry succeeds.
			ls.setDamage(damageContent)
		}
		return "", err
	}
	if err := os.Truncate(filepath.Join(lp, journalFile), 0); err != nil {
		if !os.IsNotExist(err) {
			ls.setDamage(damageContent) // new base published, old journal beside it
			return "", err
		}
	} else if err := syncJournalTruncate(filepath.Join(lp, journalFile)); err != nil {
		// Durable before acknowledgment, or the old journal can outlive
		// power loss and replay over the acknowledged replacement —
		// resurrecting the very entries the Replace removed. See Compact
		// for the identical reasoning, damage containment included.
		ls.setDamage(damageContent)
		return "", err
	}
	l.setJournalHash(newJournalHash()) // truncated: the running hash restarts with the file
	ls.setDamage(damageNone)
	l.SetBaseID(id) // before install: the base stamp carries the new hash
	l.installBase(seg)
	st.saveArtifact(l, lp, seg.ng, seg.ex)
	return l.snap.Load().version, nil
}

// persistBase writes entries to a temp file, then removes any base.idx (the
// stale-artifact guard — see Compact step 3), then renames the temp file to
// base.jsonl. A temp-write error (including an oversize entry) leaves every
// real file untouched. Returns the new base content's identity (hash of the
// written bytes) for version stamps, and whether the rename PUBLISHED the
// new base — from that point the operation cannot roll back, so a caller
// seeing published=true with an error must contain the disk/memory split
// (mark the list damaged) rather than carry on writable.
func (st *Store) persistBase(lp string, entries []Entry) (id string, published bool, err error) {
	tmp, id, err := writeBaseTemp(lp, entries)
	if err != nil {
		return "", false, err
	}
	defer os.Remove(tmp) // no-op after a successful rename
	if err := os.Remove(filepath.Join(lp, idxFile)); err != nil && !os.IsNotExist(err) {
		return "", false, err
	}
	if err := os.Rename(tmp, filepath.Join(lp, baseFile)); err != nil {
		return "", false, err
	}
	// Fsync the directory: the rename itself isn't power-loss-durable until
	// the directory entry is flushed (the fsynced tier's completing step).
	return id, true, syncDir(lp)
}

// buildInfo converts the artifact package's record into the engine's.
func buildInfo(b *artifact.BuildInfo) *IndexBuildInfo {
	if b == nil {
		return nil
	}
	return &IndexBuildInfo{Entries: b.Entries, DroppedKeys: b.DroppedKeys, KeylessEntries: b.KeylessEntries}
}

// saveArtifact persists whichever base index the list's mode produced (at
// most one of ng/ex is non-nil; both nil means no base — nothing to save) as
// the list's base.idx, recording the digest of l's analyzer spec so the
// install path can detect an analyzer change (see installWithArtifact).
// Non-fatal by design — see Compact/Replace step comments.
func (st *Store) saveArtifact(l *List, lp string, ng *ngram.Index, ex *exact.Index) {
	digest := AnalyzerSpecDigest(l.an)
	// The counters are recorded WITH the index because they cannot be
	// recovered from it: a reload that skips analysis has no way to
	// recompute them, and inferring them from the index misattributes
	// analyzer loss as a stale artifact (see IndexBuildInfo).
	dropped, keyless := l.BuildStats()
	entries, _, _ := l.Stats()
	build := artifact.BuildInfo{BaseID: l.BaseIDForArtifact(), Entries: entries, DroppedKeys: dropped, KeylessEntries: keyless}
	var err error
	switch {
	case ng != nil:
		err = artifact.Save(filepath.Join(lp, idxFile), ng, digest, build)
	case ex != nil:
		err = artifact.SaveExact(filepath.Join(lp, idxFile), ex, digest, build)
	default:
		return
	}
	if err != nil && st.OnArtifactError != nil {
		st.OnArtifactError(l.Name(), err)
	}
}

// List returns the named list. Never blocks on mutation locks: safe to call
// on the query path while any list (including this one) is being compacted.
// ErrStoreClosed is returned by every mutation once Close has run.
var ErrStoreClosed = errors.New("store: closed")

// beginOp admits one mutation, or refuses it once the store is closed. The
// caller must endOp when it finishes.
//
// Registration happens under the same lock that Close sets `closed` under,
// and — this is the part that matters — it happens BEFORE the mutation
// resolves its list or takes any per-list lock. An earlier version merely
// checked a flag here and let Close sweep the per-list locks afterwards,
// which proved nothing: a mutation that had passed the check but not yet
// reached its lock was invisible to the sweep, and wrote after Close
// returned. Holding a WaitGroup slot across the whole mutation makes the
// wait exhaustive by construction instead of by argument.
func (st *Store) beginOp() error {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return ErrStoreClosed
	}
	st.ops.Add(1)
	st.mu.Unlock()
	if testHookAdmitted != nil {
		testHookAdmitted()
	}
	return nil
}

// testHookAdmitted runs after a mutation is admitted and before it resolves
// its list or takes any lock — precisely the window Close must cover, and
// the one an earlier implementation left open. Tests set it; it is nil in
// every other build and library code never assigns it.
var testHookAdmitted func()

func (st *Store) endOp() { st.ops.Done() }

// spawnBG starts background work unless the store is closed, on the same
// counter and under the same rule as beginOp.
func (st *Store) spawnBG(f func()) bool {
	if err := st.beginOp(); err != nil {
		return false
	}
	go func() {
		defer st.endOp()
		f()
	}()
	return true
}

// Close releases a store's claim on its data directory: it refuses further
// mutations and returns only once nothing of this store's will ever write
// there again. Reads keep working — in-flight queries hold live *List
// objects and must be allowed to finish — and nothing on disk is touched,
// so a later Open of the same directory sees exactly the acknowledged
// state.
//
// It exists because two *Store on one directory silently destroy data: the
// abandoned one's auto-compaction folds ITS view of the list and truncates
// the journal, discarding operations the other store has already
// acknowledged. Anything that stops using a store while the directory may
// be reopened — kurnd dropping a tenant from its registry, then the
// operator adding it back — must Close it first.
//
// Close is idempotent and safe to call concurrently with anything else.
func (st *Store) Close() error {
	st.mu.Lock()
	if st.closed {
		// A second caller must not mistake "someone is closing" for "the
		// directory is released" — it waits for the active drain, so every
		// caller's return means the same thing.
		done := st.closeDone
		st.mu.Unlock()
		<-done
		return nil
	}
	st.closed = true
	st.closeDone = make(chan struct{})
	done := st.closeDone
	st.mu.Unlock()

	// Every mutation and background compaction holds a counter slot from
	// before it touches a list until after its last write, and none can be
	// admitted once closed is set — so this wait is the whole guarantee.
	st.ops.Wait()
	// The interval-mode group committer (if it ever started) can now stop:
	// every groupCommit wait ran inside an ops slot, so after the drain no
	// request is in flight and none can arrive. Reading fsyncCh here is
	// race-free for the same reason — it is written inside an ops slot the
	// drain has waited out. Waiting for the committer's exit makes Close's
	// contract literal: nothing of this store's runs after it returns.
	if st.fsyncCh != nil {
		close(st.fsyncStop)
		<-st.fsyncStopped
	}
	close(done)
	return nil
}

// DamagedList reports one list currently refusing append-path mutations
// (see ErrListDamaged): which list, and which repair will actually clear
// the state — readiness surfaces both, because a node whose restart would
// load a different generation than it serves must not claim ready.
type DamagedList struct {
	List   string
	Repair string
}

// DamagedLists enumerates damaged lists in name order. Lock-free with
// respect to mutation locks: a long compact must not block a status probe.
func (st *Store) DamagedLists() []DamagedList {
	st.mu.RLock()
	states := make(map[string]*listState, len(st.lists))
	for n, ls := range st.lists {
		states[n] = ls
	}
	st.mu.RUnlock()
	names := make([]string, 0, len(states))
	for n := range states {
		names = append(names, n)
	}
	slices.Sort(names)
	var out []DamagedList
	for _, n := range names {
		switch states[n].getDamage() {
		case damageCreate:
			out = append(out, DamagedList{List: n, Repair: "a create/replace died mid-window; PUT-recreate the list"})
		case damageContent:
			out = append(out, DamagedList{List: n, Repair: "retry the failed replace or compact (or PUT-recreate the list)"})
		}
	}
	return out
}

func (st *Store) List(name string) (*List, bool) {
	st.mu.RLock()
	ls := st.lists[name]
	st.mu.RUnlock()
	if ls == nil {
		return nil, false
	}
	l := ls.l.Load()
	return l, l != nil
}

// Lists returns all lists in stable (name-sorted) order. Non-blocking, like
// List.
func (st *Store) Lists() []*List {
	st.mu.RLock()
	states := make(map[string]*listState, len(st.lists))
	for n, ls := range st.lists {
		states[n] = ls
	}
	st.mu.RUnlock()
	names := make([]string, 0, len(states))
	for n := range states {
		names = append(names, n)
	}
	slices.Sort(names)
	out := make([]*List, 0, len(names))
	for _, n := range names {
		if l := states[n].l.Load(); l != nil {
			out = append(out, l)
		}
	}
	return out
}

func (st *Store) listPath(name string) string { return filepath.Join(st.dir, name) }

// fileWrite is a test seam for injecting partial-write failures into journal
// appends; production always uses (*os.File).Write. CAUTION: package-global
// mutable state — a test that overrides it races any parallel test doing
// journal appends. Tests that touch it must not use t.Parallel(), and must
// restore it before returning (the existing torn-tail tests do both).
var fileWrite = (*os.File).Write

// fileTruncate is the same kind of seam for the rollback truncate: the
// journal-damaged path only exists when a failed append ALSO fails to roll
// back, which no real filesystem produces on demand.
var fileTruncate = (*os.File).Truncate

// ErrListDamaged reports that this process can no longer vouch for the
// list's disk state — a failed append could not be rolled back, or a
// destructive operation failed after publishing disk changes it cannot
// undo. Append-path mutations are refused (reads keep serving the last
// acknowledged snapshot) until a repair succeeds; the wrapped message
// names which repair applies. Content damage (failed append rollback,
// failed Replace/Compact tail) is repaired by any successful destructive
// rebuild — Replace, Compact, or re-create. Create damage (a PUT window
// that died after its wipe began) is repaired ONLY by a successful
// re-create: the directory may hold the new declaration's config while
// memory serves the old list, and the .creating marker survives, so
// Replace and Compact refuse rather than acknowledge a partial repair.
// Reload is not a repair in either case — it refuses non-empty journals
// by ship discipline and marker-bearing directories outright. Match with
// errors.Is.
var ErrListDamaged = errors.New("store: list left in an unverifiable disk/memory state by a failed operation")

func (ls *listState) setDamage(k damageKind) { ls.damage.Store(uint32(k)) }
func (ls *listState) getDamage() damageKind  { return damageKind(ls.damage.Load()) }

// damageErr renders the list's damage as an ErrListDamaged-wrapping error
// whose text names the repair that will actually work.
func (ls *listState) damageErr() error {
	if ls.getDamage() == damageCreate {
		return fmt.Errorf("%w: a create/replace of the list died mid-window (its directory may carry the new declaration and the .creating marker); only a successful PUT re-create repairs it", ErrListDamaged)
	}
	return fmt.Errorf("%w: a successful replace, compact, or re-create repairs it", ErrListDamaged)
}

// appendJournal appends recs as JSON lines in a single write, then applies
// the store's JournalFsync policy (none / fsync-now / group commit) before
// the caller acknowledges. Caller holds ls.lock and l.mu.
//
// Every record is size-bounded BEFORE anything touches disk: readJournal
// refuses lines over maxLine, and an acknowledged write must never brick the
// next Open, so an oversize entry is rejected here (by ID) with nothing
// persisted.
//
// The invariant on EVERY return is that the file's bytes equal the bytes
// l.jhash has hashed: the running hash is the version stamp's journal
// identity, so file and hash must never diverge. Success extends the hash
// with exactly the appended bytes. Any failure after bytes may have reached
// the file (partial write, fsync, group commit) rolls the file back to its
// pre-append size — the record was never acknowledged, so removing it is
// legal under at-least-once — which also keeps the tail newline-terminated
// (a glued-on fragment would make torn-tail repair silently drop the NEXT
// acknowledged append). If the rollback itself fails the invariant is
// unrecoverable in place: the list is marked damaged and further
// appends are refused until Replace/Compact/ReloadList rebuild the file.
//
// Returns the journal byte position just past the appended records — the
// replay-depth half of the version stamp.
func (st *Store) appendJournal(ls *listState, l *List, path string, recs []journalRec) (int64, error) {
	if ls.getDamage() != damageNone {
		return 0, ls.damageErr()
	}
	var buf []byte
	for i := range recs {
		line, err := json.Marshal(&recs[i])
		if err != nil {
			return 0, err
		}
		if len(line)+1 > maxLine {
			id := recs[i].ID
			if recs[i].Entry != nil {
				id = recs[i].Entry.ID
			}
			return 0, &EntryTooLargeError{ID: id, Size: len(line) + 1}
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	start, err := f.Seek(0, io.SeekEnd) // O_APPEND: end == append position
	if err != nil {
		f.Close()
		return 0, err
	}
	rollback := func(werr error) (int64, error) {
		if terr := fileTruncate(f, start); terr != nil {
			ls.setDamage(damageContent)
		}
		f.Close()
		return 0, werr
	}
	if _, werr := fileWrite(f, buf); werr != nil {
		return rollback(werr)
	}
	if st.JournalFsync == FsyncEvery {
		if serr := f.Sync(); serr != nil {
			return rollback(serr)
		}
	}
	if cerr := f.Close(); cerr != nil {
		// The write may or may not have reached the file; reopen to roll back.
		if terr := os.Truncate(path, start); terr != nil {
			ls.setDamage(damageContent)
		}
		return 0, cerr
	}
	if st.JournalFsync == FsyncInterval {
		if gerr := st.groupCommit(path); gerr != nil {
			if terr := os.Truncate(path, start); terr != nil {
				ls.setDamage(damageContent)
			}
			return 0, gerr
		}
	}
	if l.jhash != nil {
		l.jhash.Write(buf)
	}
	return start + int64(len(buf)), nil
}

// readBase loads base.jsonl. haveBase is false when the file doesn't exist
// (fresh list, no base segment). id is the content identity of the bytes
// read — the same hash persistBase computed when it wrote them, so versions
// survive restarts.
func readBase(path string) (entries []Entry, haveBase bool, id string, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, "", nil
		}
		return nil, false, "", err
	}
	defer f.Close()
	h := sha256.New()
	sc := bufio.NewScanner(io.TeeReader(f, h))
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	ln := 0
	for sc.Scan() {
		ln++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, false, "", fmt.Errorf("%s:%d: %w", filepath.Base(path), ln, err)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, false, "", fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return entries, true, baseIDFromHash(h), nil
}

// readJournal loads journal records. The journal is written without fsync, so
// a crash can leave a torn final line. Replay keeps everything up to the last
// intact record (records are appended whole, so a parse failure or a missing
// trailing newline marks the torn tail) instead of failing startup; tornAt is
// the byte offset just past that record (-1 when the file is clean or
// missing) so the caller can truncate the fragment away — otherwise the next
// append would glue its record onto the torn line, and that acknowledged
// write would be dropped by the following restart. A newline-less final
// record is dropped even when it parses as JSON: its write never completed,
// so it was never acknowledged, and keeping it would leave the file unsafe to
// append to. A missing journal is simply empty.
func readJournal(path string) (ops []journalRec, tornAt int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, -1, nil
		}
		return nil, -1, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	var off int64 // end of the last intact (newline-terminated, parseable) record
	for {
		line, rerr := readBoundedLine(r, maxLine)
		if rerr == errJournalLineTooLong {
			// Every writer size-checks before appending, so an oversize
			// record is corruption by definition. Treat it exactly like an
			// unparseable one — keep the prefix that replayed cleanly and
			// let the caller truncate — instead of failing openList, which
			// would skip the WHOLE list: harsher than the quarantine an
			// merely unparseable journal gets, for a strictly worse file.
			return ops, off, nil
		}
		if rerr != nil {
			if rerr == io.EOF {
				if len(line) > 0 {
					return ops, off, nil // torn tail: no trailing newline
				}
				return ops, -1, nil // clean EOF
			}
			return nil, -1, fmt.Errorf("%s: %w", filepath.Base(path), rerr)
		}

		body := line[:len(line)-1] // strip '\n'
		if len(body) > 0 {
			var rec journalRec
			if err := json.Unmarshal(body, &rec); err != nil {
				return ops, off, nil // corrupt record: keep what replayed cleanly
			}
			ops = append(ops, rec)
		}
		off += int64(len(line))
	}
}

var errJournalLineTooLong = errors.New("journal record exceeds the line bound")

// readBoundedLine reads one newline-terminated record without materializing
// more than max bytes. ReadBytes would slurp first and let the caller judge
// afterwards, so a journal whose tail is one enormous newline-less blob (a
// truncated write, a corrupted file) was read whole at Open purely to be
// thrown away. Returns io.EOF alongside a final unterminated record, as
// ReadBytes does.
func readBoundedLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, rerr := r.ReadSlice('\n')
		if len(line)+len(chunk) > max {
			return nil, errJournalLineTooLong
		}
		line = append(line, chunk...) // chunk is valid only until the next read
		if rerr == bufio.ErrBufferFull {
			continue
		}
		return line, rerr
	}
}

// writeBaseTemp writes entries as JSON lines to a fsynced temp file in dir
// and returns its path; the caller renames it into place (the split lets
// callers order the stale-artifact removal between write and rename). Any
// error — including an entry whose line would exceed maxLine, which readBase
// would refuse to load on the next Open — cleans up the temp file and leaves
// nothing behind.
func writeBaseTemp(dir string, entries []Entry) (path, id string, err error) {
	tmp, err := os.CreateTemp(dir, ".base-*")
	if err != nil {
		return "", "", err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	// Hash exactly the bytes written: the file IS the identity, so a reopen
	// hashing base.jsonl (readBase) reproduces the same id.
	h := sha256.New()
	w := bufio.NewWriterSize(io.MultiWriter(tmp, h), 1<<20)
	for i := range entries {
		line, err := json.Marshal(&entries[i])
		if err != nil {
			return "", "", err
		}
		if len(line)+1 > maxLine {
			return "", "", &EntryTooLargeError{ID: entries[i].ID, Size: len(line) + 1}
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return "", "", err
		}
	}
	if err := w.Flush(); err != nil {
		return "", "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	return tmp.Name(), baseIDFromHash(h), nil
}

// AnalyzerSpecDigest is the identity of an analyzer's canonical step spec:
// the complete sha256 in hex (same rationale as baseIDFromHash — a
// truncated digest invites deliberate collisions, and a collision here
// silently installs a stale index). Artifacts record it at save time so
// installWithArtifact can verify that index keys and future query strings
// are normalized by the same analyzer; a mismatch — or a pre-digest /
// short-digest artifact — forces a full rebuild.
//
// Each step is length-prefixed into the hash: step ARGUMENTS contain commas
// ("strip_words:mr,mrs"), so any separator-join would make distinct specs
// collide (["strip_words:mr,trim"] vs ["strip_words:mr","trim"]) — and a
// digest collision here silently installs a stale index, the exact failure
// the digest exists to prevent.
func AnalyzerSpecDigest(a analyzer.Analyzer) string {
	h := sha256.New()
	var lb [8]byte
	for _, s := range a.Steps() {
		binary.BigEndian.PutUint64(lb[:], uint64(len(s)))
		h.Write(lb[:])
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// baseIDFromHash renders a content hash as a version-stamp identity: the
// COMPLETE sha256, not a prefix. The stamp is presented as evidence that
// verifies itself (an archived base.jsonl or journal re-hashes to its
// stamp), and evidence must survive an adversarial publisher: a truncated
// prefix turns that claim into a birthday problem — 48 bits fell to ~2^24
// deliberate-collision work. Log readability does not justify weakening
// identity; readers may display prefixes, the stamp itself carries it all.
// (Bundle manifests keep their 12-hex version_id as a display/join key: it
// remains a prefix of this full hash, and the manifest carries the full
// sha256 alongside.)
func baseIDFromHash(h hash.Hash) string {
	return hex.EncodeToString(h.Sum(nil))
}

// newJournalHash starts the running hash of a journal's byte prefix (the
// overlay half of the version stamp — see List.jhash). Domain-separated from
// the base hash: base.jsonl and journal.jsonl with coincidentally equal
// bytes must not produce equal identities in the two halves of one stamp.
func newJournalHash() hash.Hash {
	h := sha256.New()
	h.Write([]byte("kurn journal v1\x00"))
	return h
}

// writeFileAtomic writes data via temp+rename (config.json), fsyncing the
// file and then the containing directory — the rename isn't power-loss-
// durable until the directory entry is flushed.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cfg-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
