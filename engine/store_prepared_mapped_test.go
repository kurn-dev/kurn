//go:build unix

package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/kurn-dev/kurn/bench"
	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/prepared"
	"github.com/kurn-dev/kurn/engine/prepared/builder"
)

// Tests of the mapped loader (LoaderMapped). A process admits one mapped
// artifact for its whole lifetime, so every mapped open runs in a child:
// this test binary re-executed with KURN_MAPPED_CHILD naming the scenario
// and KURN_MAPPED_* carrying its inputs. The parent never opens a mapped store, and
// each child starts with an empty registry. Under -race the children are
// race binaries too.

const mappedChildEnv = "KURN_MAPPED_CHILD"

func runMappedChild(t *testing.T, mode string, env map[string]string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPreparedMappedChild$", "-test.v", "-test.count=1", "-test.timeout=5m")
	cmd.Env = append(os.Environ(), mappedChildEnv+"="+mode)
	for k, v := range env {
		cmd.Env = append(cmd.Env, "KURN_MAPPED_"+k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// mustMappedChild runs a child scenario that must pass.
func mustMappedChild(t *testing.T, mode string, env map[string]string) string {
	t.Helper()
	out, err := runMappedChild(t, mode, env)
	if err != nil || !strings.Contains(out, "--- PASS: TestPreparedMappedChild") {
		t.Fatalf("child %s: %v\n%s", mode, err, out)
	}
	return out
}

func mappedEnv(k string) string { return os.Getenv("KURN_MAPPED_" + k) }

var mappedOpts = engine.PreparedOptions{Loader: engine.LoaderMapped}

// TestPreparedMappedChild is the child side; it runs only when re-executed
// by a parent test.
func TestPreparedMappedChild(t *testing.T) {
	mode := os.Getenv(mappedChildEnv)
	if mode == "" {
		t.Skip("runs only as a child of the mapped-loader tests")
	}
	switch mode {
	case "oracle":
		mappedChildOracle(t)
	case "fault":
		mappedChildFault(t)
	case "preconditions":
		mappedChildPreconditions(t)
	case "reuse":
		mappedChildReuse(t)
	case "inplace":
		mappedChildInPlace(t)
	case "concurrent-same":
		mappedChildConcurrent(t, false)
	case "concurrent-two":
		mappedChildConcurrent(t, true)
	case "failed":
		mappedChildFailed(t)
	case "close":
		mappedChildClose(t)
	case "accept":
		mappedChildAccept(t)
	case "tm-none", "tm-full", "tm-trusted", "tm-race-mixed", "tm-race-first", "tm-nofallback":
		mappedChildTrusted(t, mode)
	case "sanity":
		mappedChildSanity(t)
	default:
		t.Fatalf("unknown child mode %q", mode)
	}
}

// mappedOracleRecord is what the oracle child reports.
type mappedOracleRecord struct {
	Answers []byte // prepAnswers, byte for byte
	Costs   []int64
	Info    engine.PreparedInfo
	Version string
}

// (a) Oracle equivalence: for every case (n-gram, exact, filtered, with
// and without payload gaps) every query, filtered query, typed-filter
// query and emulated batch, and every read accessor, of a mapped store is
// byte-identical to the heap-loaded store's, under the same stamp and the
// same verified identity. The child also re-runs L2's read-only checks on
// the mapped store.
func TestPreparedMappedOracleEquivalence(t *testing.T) {
	for _, c := range prepCases() {
		t.Run(c.name, func(t *testing.T) {
			path := prepWriteArtifact(t, c)
			heap := prepOpen(t, path, engine.PreparedOptions{})
			hl := prepList(t, heap)
			want := prepAnswers(t, hl, c)
			prepCheckCoverage(t, want, c)

			out := filepath.Join(t.TempDir(), "mapped.json")
			mustMappedChild(t, "oracle", map[string]string{"CASE": c.name, "PATH": path, "OUT": out})
			raw, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			var got mappedOracleRecord
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Answers, want) {
				t.Fatalf("mapped answers differ from heap answers:\n%s", prepFirstDiff(got.Answers, want))
			}
			if got.Version != hl.Version() {
				t.Fatalf("mapped stamp %s, heap %s", got.Version, hl.Version())
			}
			hi := heap.Prepared()
			if got.Info.Loader != engine.LoaderMapped || hi.Loader != engine.LoaderHeap || !got.Info.IdentitiesRecomputed {
				t.Fatalf("loaders: mapped info %+v, heap info %+v", got.Info, hi)
			}
			if got.Info.Verified != hi.Verified || got.Info.Level != prepared.LevelFull || got.Info.ContainerSHA256 == "" {
				t.Fatalf("verified: mapped %+v, heap %+v", got.Info.Verified, hi.Verified)
			}
			if len(c.filters) > 0 {
				if wantC := prepFilteredCosts(t, hl, c); fmt.Sprint(got.Costs) != fmt.Sprint(wantC) {
					t.Fatalf("filtered costs: mapped %v, heap %v", got.Costs, wantC)
				}
			}
		})
	}
}

func mappedChildOracle(t *testing.T) {
	c := prepCaseNamed(t, mappedEnv("CASE"))
	path := mappedEnv("PATH")
	dir := filepath.Dir(path)
	before := prepSnapshotDir(t, dir)
	st, err := engine.OpenPrepared(path, mappedOpts)
	if err != nil {
		t.Fatal(err)
	}
	l := prepList(t, st)
	rec := mappedOracleRecord{Answers: prepAnswers(t, l, c), Info: *st.Prepared(), Version: l.Version()}
	if len(c.filters) > 0 {
		rec.Costs = prepFilteredCosts(t, l, c)
	}
	if !st.ReadOnly() || l.BaseNgram() != nil || l.BaseExact() != nil || st.ReadOnlyRefusals() != 0 {
		t.Fatalf("mapped store: ReadOnly %v, base indexes handed out, or refusals %d", st.ReadOnly(), st.ReadOnlyRefusals())
	}
	// L2's read-only guarantees hold unchanged on the mapped store.
	errCalls, noops := prepMutators(st, l, prepListName)
	for what, call := range errCalls {
		if err := call(); !errors.Is(err, engine.ErrReadOnly) {
			t.Errorf("%s: %v, want ErrReadOnly", what, err)
		}
	}
	for _, call := range noops {
		call()
	}
	if got, want := st.ReadOnlyRefusals(), uint64(len(errCalls)+len(noops)); got != want {
		t.Fatalf("ReadOnlyRefusals %d, want %d", got, want)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if got := prepAnswers(t, l, c); !bytes.Equal(got, rec.Answers) {
		t.Fatalf("answers changed after refused mutations and Close:\n%s", prepFirstDiff(got, rec.Answers))
	}
	if after := prepSnapshotDir(t, dir); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("artifact directory changed:\nbefore %v\nafter  %v", before, after)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mappedEnv("OUT"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// mappedPayloads runs every query path of l over c and returns, per path,
// the non-empty payload slices of its results (at most 64 per path).
func mappedPayloads(t testing.TB, l *engine.List, c prepCase) map[string][][]byte {
	t.Helper()
	ctx := context.Background()
	all := engine.QueryOpts{Threshold: -1, TopK: -1}
	out := map[string][][]byte{}
	add := func(path string, cs []engine.Candidate) {
		for _, cd := range cs {
			if len(cd.Payload) > 0 && len(out[path]) < 64 {
				out[path] = append(out[path], cd.Payload)
			}
		}
	}
	var batch []*engine.PreparedQuery
	for _, q := range c.queries {
		add("Query", l.Query(q, all))
		add("QueryCtx", l.QueryCtx(ctx, q, all))
		cs, _ := l.QueryVersioned(ctx, q, all)
		add("QueryVersioned", cs)
		cs, _ = l.PrepareQuery(q, all).Execute(ctx)
		add("PrepareQuery.Execute", cs)
		batch = append(batch, l.PrepareQuery(q, engine.QueryOpts{}))
		for _, f := range c.filters {
			if cs, _, err := l.QueryFilteredCtx(ctx, q, all, f); err == nil {
				add("QueryFilteredCtx", cs)
			}
			if p, err := l.PrepareFilteredQuery(q, all, f); err == nil {
				if cs, _, err := p.Execute(ctx); err == nil {
					add("PrepareFilteredQuery.Execute", cs)
				}
			}
		}
		for _, raw := range c.typed {
			tf, err := engine.ParseTypedFilter([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if cs, _, err := l.QueryTypedFilteredCtx(ctx, q, all, tf); err == nil {
				add("QueryTypedFilteredCtx", cs)
			}
			if p, err := l.PrepareTypedFilteredQuery(q, all, tf); err == nil {
				if cs, _, _, err := p.ExecuteStats(ctx); err == nil {
					add("PrepareTypedFilteredQuery.ExecuteStats", cs)
				}
			}
		}
	}
	for _, p := range batch {
		cs, _ := p.Execute(ctx)
		add("batch", cs)
	}
	for _, e := range l.LiveEntries() {
		if len(e.Payload) > 0 && len(out["LiveEntries"]) < 64 {
			out["LiveEntries"] = append(out["LiveEntries"], e.Payload)
		}
	}
	return out
}

var mappedPaths = []string{"Query", "QueryCtx", "QueryVersioned", "PrepareQuery.Execute", "batch", "QueryFilteredCtx",
	"PrepareFilteredQuery.Execute", "QueryTypedFilteredCtx", "PrepareTypedFilteredQuery.ExecuteStats", "LiveEntries"}

// writeFaults reports whether writing p[0] faults at p's address.
func writeFaults(p []byte) (faulted bool) {
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	defer func() {
		if e := recover(); e != nil {
			ae, ok := e.(interface{ Addr() uintptr })
			if !ok || ae.Addr() != uintptr(unsafe.Pointer(unsafe.SliceData(p))) {
				panic(e)
			}
			faulted = true
		}
	}()
	p[0] ^= 0xff
	return false
}

var faultAddrRE = regexp.MustCompile(`unexpected fault address (0x[0-9a-f]+)`)

// (b) The mapping is read-only: in a child, a write into a result payload
// from every query path faults (caught per path with SetPanicOnFault);
// then an unprotected write crashes the child with SIGBUS or SIGSEGV at
// the payload's address, which the parent asserts.
func TestPreparedMappedReadOnlyFault(t *testing.T) {
	for _, name := range []string{"ngram-filtered", "exact-filtered"} {
		t.Run(name, func(t *testing.T) {
			path := prepWriteArtifact(t, prepCaseNamed(t, name))
			out, err := runMappedChild(t, "fault", map[string]string{"CASE": name, "PATH": path})
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != 2 {
				t.Fatalf("child did not crash with the runtime's fault exit (err %v):\n%s", err, out)
			}
			if !strings.Contains(out, "MAPPED-EVERY-PATH-FAULTED") || strings.Contains(out, "MAPPED-WRITE-SUCCEEDED") {
				t.Fatalf("child output:\n%s", out)
			}
			if !strings.Contains(out, "fatal error: fault") || !(strings.Contains(out, "[signal SIGBUS") || strings.Contains(out, "[signal SIGSEGV")) {
				t.Fatalf("no SIGBUS/SIGSEGV fault in the child's crash:\n%s", out)
			}
			m := faultAddrRE.FindStringSubmatch(out)
			target := regexp.MustCompile(`MAPPED-WRITING-UNPROTECTED (0x[0-9a-f]+)`).FindStringSubmatch(out)
			if m == nil || target == nil || m[1] != target[1] {
				t.Fatalf("fault address %v, payload written at %v", m, target)
			}
			t.Logf("%s", regexp.MustCompile(`MAPPED-EVERY-PATH-FAULTED.*`).FindString(out))
			t.Logf("child crashed as required: %s", regexp.MustCompile(`\[signal [^\]]*\]`).FindString(out))
		})
	}
}

func mappedChildFault(t *testing.T) {
	c := prepCaseNamed(t, mappedEnv("CASE"))
	st, err := engine.OpenPrepared(mappedEnv("PATH"), mappedOpts)
	if err != nil {
		t.Fatal(err)
	}
	l := prepList(t, st)
	byPath := mappedPayloads(t, l, c)
	var counts []string
	for _, p := range mappedPaths {
		pls := byPath[p]
		if len(pls) == 0 {
			t.Fatalf("query path %s returned no payload to write", p)
		}
		for _, pl := range pls {
			if !writeFaults(pl) {
				t.Fatalf("%s: a write into a result payload did not fault", p)
			}
		}
		counts = append(counts, fmt.Sprintf("%s=%d", p, len(pls)))
	}
	fmt.Printf("MAPPED-EVERY-PATH-FAULTED %s\n", strings.Join(counts, " "))
	target := byPath["Query"][0]
	fmt.Printf("MAPPED-WRITING-UNPROTECTED %#x\n", uintptr(unsafe.Pointer(unsafe.SliceData(target))))
	os.Stdout.Sync()
	target[0] ^= 0xff
	fmt.Println("MAPPED-WRITE-SUCCEEDED")
	t.Fatal("an unprotected write into the mapping did not crash")
}

// (c) A file with write bits, a directory, a character device, an empty
// file, and a structure-level admission are refused without mapping
// anything, and a later valid open still admits.
func TestPreparedMappedPreconditions(t *testing.T) {
	c := prepCases()[0]
	path := prepWriteArtifact(t, c)
	dir := t.TempDir()
	writable := filepath.Join(dir, "w.kart")
	prepCopyFile(t, path, writable) // 0644
	empty := filepath.Join(dir, "empty.kart")
	if err := os.WriteFile(empty, nil, 0o444); err != nil {
		t.Fatal(err)
	}
	out := mustMappedChild(t, "preconditions", map[string]string{"VALID": path, "WRITABLE": writable, "DIR": dir, "EMPTY": empty})
	t.Log(strings.Join(regexp.MustCompile(`(?m)^.*refused: .*$`).FindAllString(out, -1), "\n"))
}

func mappedChildPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		level      prepared.Level
		msg        string
		is         error
	}{
		{"write bits", mappedEnv("WRITABLE"), prepared.LevelFull, "write permission bits set", engine.ErrNotMappable},
		{"directory", mappedEnv("DIR"), prepared.LevelFull, "not a regular file", engine.ErrNotMappable},
		{"character device", os.DevNull, prepared.LevelFull, "not a regular file", engine.ErrNotMappable},
		{"empty", mappedEnv("EMPTY"), prepared.LevelFull, "is empty", prepared.ErrMalformed},
		{"structure-level admission", mappedEnv("VALID"), prepared.LevelStructure, "requires full or trusted verification", engine.ErrNotMappable},
	} {
		st, err := engine.OpenPrepared(tc.path, engine.PreparedOptions{Loader: engine.LoaderMapped, Verify: tc.level})
		if st != nil || err == nil || !strings.Contains(err.Error(), tc.msg) || errors.Is(err, engine.ErrMappedInUse) {
			t.Fatalf("%s: store %v err %v, want a refusal mentioning %q", tc.name, st, err, tc.msg)
		}
		if !errors.Is(err, tc.is) || errors.Is(err, prepared.ErrNotMappable) != (tc.is == engine.ErrNotMappable) {
			t.Fatalf("%s: %v is not (only) %v", tc.name, err, tc.is)
		}
		t.Logf("%s refused: %v", tc.name, err)
	}
	st, err := engine.OpenPrepared(mappedEnv("VALID"), mappedOpts)
	if err != nil {
		t.Fatalf("valid open after refusals: %v", err)
	}
	if st.Prepared().Loader != engine.LoaderMapped {
		t.Fatalf("info %+v", st.Prepared())
	}
	// Now something is admitted: the writable file is another file.
	if _, err := engine.OpenPrepared(mappedEnv("WRITABLE"), mappedOpts); !errors.Is(err, engine.ErrMappedInUse) {
		t.Fatalf("writable file after admission: %v, want ErrMappedInUse", err)
	}
}

// payloadAddrs returns the address of every non-empty payload of l's
// entries, in ID order.
func payloadAddrs(l *engine.List) []uintptr {
	var out []uintptr
	for _, e := range l.LiveEntries() {
		if len(e.Payload) > 0 {
			out = append(out, uintptr(unsafe.Pointer(unsafe.SliceData(e.Payload))))
		}
	}
	return out
}

// (d) Admission and reuse: the same file again is served from the same
// mapping (also at LevelStructure, reported as the full verification the
// reuse ran); a different valid artifact and a replacement by rename are
// refused with ErrMappedInUse; open stores keep answering.
func TestPreparedMappedAdmissionAndReuse(t *testing.T) {
	a := prepWriteArtifact(t, prepCases()[0])
	b := prepWriteArtifact(t, prepCases()[1])
	mustMappedChild(t, "reuse", map[string]string{"A": a, "B": b})
}

func mappedChildReuse(t *testing.T) {
	c := prepCases()[0]
	a, b := mappedEnv("A"), mappedEnv("B")
	s1, err := engine.OpenPrepared(a, mappedOpts)
	if err != nil {
		t.Fatal(err)
	}
	want := prepAnswers(t, prepList(t, s1), c)
	s2, err := engine.OpenPrepared(a, mappedOpts)
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	p1, p2 := payloadAddrs(prepList(t, s1)), payloadAddrs(prepList(t, s2))
	if len(p1) == 0 || fmt.Sprint(p1) != fmt.Sprint(p2) {
		t.Fatalf("reuse is not served from the admitted mapping: %d payloads, first %#x vs %#x", len(p1), p1[0], p2[0])
	}
	if *s1.Prepared() != *s2.Prepared() {
		t.Fatalf("reuse info %+v, admission %+v", s2.Prepared(), s1.Prepared())
	}
	s3, err := engine.OpenPrepared(a, engine.PreparedOptions{Loader: engine.LoaderMapped, Verify: prepared.LevelStructure})
	if err != nil {
		t.Fatalf("structure-level reuse: %v", err)
	}
	if info := s3.Prepared(); info.Level != prepared.LevelFull || info.ContainerSHA256 != s1.Prepared().ContainerSHA256 || fmt.Sprint(payloadAddrs(prepList(t, s3))) != fmt.Sprint(p1) {
		t.Fatalf("structure-level reuse info %+v", info)
	}
	for _, lvl := range []prepared.Level{prepared.LevelFull, prepared.LevelStructure} {
		st, err := engine.OpenPrepared(b, engine.PreparedOptions{Loader: engine.LoaderMapped, Verify: lvl})
		if st != nil || !errors.Is(err, engine.ErrMappedInUse) || !errors.Is(err, prepared.ErrMappedInUse) {
			t.Fatalf("different artifact at %v: %v, want ErrMappedInUse", lvl, err)
		}
		t.Logf("different artifact refused: %v", err)
	}
	// Replace a's path by rename: a new inode with the same bytes.
	next := filepath.Join(filepath.Dir(a), "next.kart")
	prepCopyFile(t, a, next)
	if err := os.Chmod(next, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, a); err != nil {
		t.Fatal(err)
	}
	if st, err := engine.OpenPrepared(a, mappedOpts); st != nil || !errors.Is(err, engine.ErrMappedInUse) {
		t.Fatalf("replaced file: %v, want ErrMappedInUse", err)
	} else {
		t.Logf("replaced file refused: %v", err)
	}
	for i, st := range []*engine.Store{s1, s2, s3} {
		if got := prepAnswers(t, prepList(t, st), c); !bytes.Equal(got, want) {
			t.Fatalf("store %d answers changed after refusals", i+1)
		}
	}
}

// (d) A same-inode, same-length change: in a child, admit, make the file
// writable (the next open is refused), rewrite one byte of the build
// section's source string in place and restore 0444. The next open is
// refused because the admitted identity no longer verifies; the open
// store keeps answering; restoring the byte restores reuse.
func TestPreparedMappedInPlaceChange(t *testing.T) {
	path := prepWriteArtifact(t, prepCases()[0])
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := prepSections(buf)[prepared.KindBuild]
	i := bytes.Index(buf[s[0]:s[0]+s[1]], []byte(`"source":"test"`))
	if i < 0 {
		t.Fatal("no source in the build section")
	}
	off := int(s[0]) + i + len(`"source":"`)
	out := mustMappedChild(t, "inplace", map[string]string{"PATH": path, "OFF": fmt.Sprint(off)})
	t.Log(strings.Join(regexp.MustCompile(`(?m)^.*(refused|still answers).*$`).FindAllString(out, -1), "\n"))
}

func mappedChildInPlace(t *testing.T) {
	c := prepCases()[0]
	path := mappedEnv("PATH")
	var off int64
	if _, err := fmt.Sscan(mappedEnv("OFF"), &off); err != nil {
		t.Fatal(err)
	}
	s1, err := engine.OpenPrepared(path, mappedOpts)
	if err != nil {
		t.Fatal(err)
	}
	l := prepList(t, s1)
	want := prepAnswers(t, l, c)
	info := *s1.Prepared()
	open := func() error {
		st, err := engine.OpenPrepared(path, mappedOpts)
		if (st == nil) == (err == nil) {
			t.Fatalf("store %v with err %v", st, err)
		}
		return err
	}
	chmod := func(m os.FileMode) {
		if err := os.Chmod(path, m); err != nil {
			t.Fatal(err)
		}
	}
	chmod(0o644)
	if err := open(); !errors.Is(err, engine.ErrMappedInUse) || !strings.Contains(err.Error(), "write permission") {
		t.Fatalf("admitted file made writable: %v", err)
	} else {
		t.Logf("writable admitted file refused: %v", err)
	}
	chmod(0o444)
	if err := open(); err != nil {
		t.Fatalf("write bits cleared again: %v", err)
	}
	patch := func(b byte) {
		chmod(0o644)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte{b}, off); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		chmod(0o444)
	}
	patch('T') // "test" → "Test": still canonical JSON, same length
	err = open()
	if !errors.Is(err, engine.ErrMappedInUse) || !errors.Is(err, prepared.ErrIntegrity) {
		t.Fatalf("same-inode change: %v, want ErrMappedInUse (integrity)", err)
	}
	t.Logf("same-inode change refused: %v", err)
	got := prepAnswers(t, l, c)
	t.Logf("open store still answers (answers unchanged: %v; the change touched provenance only)", bytes.Equal(got, want))
	if *s1.Prepared() != info {
		t.Fatal("open store's info changed")
	}
	patch('t')
	if err := open(); err != nil {
		t.Fatalf("restored bytes: %v", err)
	}
}

// (e) Concurrency: 16 goroutines race a mapped open of one file: exactly
// one admission (every store shares one mapping) and all succeed; raced
// over two files, exactly one file wins and every open of the other gets
// ErrMappedInUse. Run under -race.
func TestPreparedMappedConcurrentOpens(t *testing.T) {
	a := prepWriteArtifact(t, prepCases()[0])
	b := prepWriteArtifact(t, prepCases()[1])
	for i := 0; i < 3; i++ {
		mustMappedChild(t, "concurrent-same", map[string]string{"A": a})
		out := mustMappedChild(t, "concurrent-two", map[string]string{"A": a, "B": b})
		t.Log(regexp.MustCompile(`(?m)^.*winner.*$`).FindString(out))
	}
}

func mappedChildConcurrent(t *testing.T, two bool) {
	const workers = 16
	files := []string{mappedEnv("A")}
	if two {
		files = append(files, mappedEnv("B"))
	}
	type res struct {
		file string
		st   *engine.Store
		err  error
	}
	out := make([]res, workers)
	var start, wg sync.WaitGroup
	start.Add(1)
	for i := range out {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start.Wait()
			f := files[i%len(files)]
			st, err := engine.OpenPrepared(f, mappedOpts)
			out[i] = res{f, st, err}
		}()
	}
	start.Done()
	wg.Wait()
	winner := ""
	var addrs string
	for _, r := range out {
		if r.err != nil {
			continue
		}
		if winner != "" && r.file != winner {
			t.Fatalf("both files admitted: %s and %s", winner, r.file)
		}
		winner = r.file
		a := fmt.Sprint(payloadAddrs(prepList(t, r.st)))
		if addrs != "" && a != addrs {
			t.Fatal("two stores of the winning file are served from different mappings")
		}
		addrs = a
	}
	if winner == "" {
		t.Fatal("no open succeeded")
	}
	for _, r := range out {
		switch {
		case r.file == winner && r.err != nil:
			t.Fatalf("winning file refused: %v", r.err)
		case r.file != winner && (r.st != nil || !errors.Is(r.err, engine.ErrMappedInUse)):
			t.Fatalf("losing file: %v, want ErrMappedInUse", r.err)
		}
	}
	name := map[string]string{mappedEnv("A"): "A", mappedEnv("B"): "B"}[winner]
	t.Logf("winner %s: %d of %d opens served from one mapping", name, workers/len(files), workers)
}

// (f) A corrupt artifact with a stale digest fails verification, is not
// admitted, and leaves the registry empty: a valid open then admits.
func TestPreparedMappedFailedAdmission(t *testing.T) {
	c := prepCases()[0]
	buf := prepEncode(t, c)
	prepPatchSection(t, buf, prepared.KindBuild, `"source":"test"`, `"source":"Test"`) // digests stale
	stale := prepWriteBytes(t, buf)
	valid := prepWriteArtifact(t, c)
	mustMappedChild(t, "failed", map[string]string{"STALE": stale, "VALID": valid})
}

func mappedChildFailed(t *testing.T) {
	stale, valid := mappedEnv("STALE"), mappedEnv("VALID")
	st, err := engine.OpenPrepared(stale, mappedOpts)
	if st != nil || !errors.Is(err, prepared.ErrIntegrity) || errors.Is(err, engine.ErrMappedInUse) {
		t.Fatalf("stale digest: %v, want ErrIntegrity only", err)
	}
	t.Logf("stale digest refused: %v", err)
	if _, err := engine.OpenPrepared(valid, mappedOpts); err != nil {
		t.Fatalf("valid open after a failed admission: %v", err)
	}
	if _, err := engine.OpenPrepared(stale, mappedOpts); !errors.Is(err, engine.ErrMappedInUse) {
		t.Fatalf("stale file after admission: %v, want ErrMappedInUse", err)
	}
}

// Accept before admit: artifacts that pass the format's full verification
// but fail the engine's own checks (C1: a declared config digest the
// engine does not recompute, ErrIntegrity; a configuration NewList
// refuses, ErrIncompatible) are refused through the registry's acceptance
// step, so nothing is admitted: each refusal is the engine's, never
// ErrMappedInUse, and a valid open afterwards admits.
func TestPreparedMappedEngineRefusalNotAdmitted(t *testing.T) {
	c := prepCases()[2] // ngram with filterable fields
	ok := prepEncode(t, c)
	_, ver, err := prepared.VerifyBytes(ok, prepared.LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	flip := []byte(ver.ConfigDigest)
	if flip[0] == '0' {
		flip[0] = '1'
	} else {
		flip[0] = '0'
	}
	env := map[string]string{"VALID": prepWriteArtifact(t, c)}
	for key, p := range map[string][2]string{
		"DIGEST": {`"config_digest":"` + ver.ConfigDigest, `"config_digest":"` + string(flip)},
		"CONFIG": {`"threshold":0.6`, `"threshold":7.6`},
	} {
		buf := prepared.AlignedBuffer(len(ok))
		copy(buf, ok)
		prepPatchSection(t, buf, prepared.KindConfig, p[0], p[1])
		prepRehash(buf)
		if _, _, err := prepared.VerifyBytes(buf, prepared.LevelFull); err != nil {
			t.Fatalf("%s: the patched artifact must pass the format's full verification: %v", key, err)
		}
		env[key] = prepWriteBytes(t, buf)
	}
	out := mustMappedChild(t, "accept", env)
	t.Log(strings.Join(regexp.MustCompile(`(?m)^.*refused: .*$`).FindAllString(out, -1), "\n"))
}

func mappedChildAccept(t *testing.T) {
	cases := []struct {
		key  string
		is   error
		want string
	}{
		{"DIGEST", prepared.ErrIntegrity, "config digest"},
		{"CONFIG", prepared.ErrIncompatible, "cannot resolve the artifact's list configuration"},
	}
	// Twice each, interleaved: had any refusal admitted its mapping, every
	// later open of another file would be ErrMappedInUse.
	for round := 0; round < 2; round++ {
		for _, tc := range cases {
			st, err := engine.OpenPrepared(mappedEnv(tc.key), mappedOpts)
			if st != nil || !errors.Is(err, tc.is) || !strings.Contains(err.Error(), tc.want) || errors.Is(err, engine.ErrMappedInUse) {
				t.Fatalf("%s: store %v err %v, want %v naming %q and no admission", tc.key, st, err, tc.is, tc.want)
			}
			t.Logf("%s refused: %v", tc.key, err)
		}
	}
	st, err := engine.OpenPrepared(mappedEnv("VALID"), mappedOpts)
	if err != nil {
		t.Fatalf("valid open after engine refusals: %v", err)
	}
	if info := st.Prepared(); info.Loader != engine.LoaderMapped || !info.IdentitiesRecomputed {
		t.Fatalf("info %+v", info)
	}
	for _, tc := range cases {
		if _, err := engine.OpenPrepared(mappedEnv(tc.key), mappedOpts); !errors.Is(err, engine.ErrMappedInUse) {
			t.Fatalf("%s after admission: %v, want ErrMappedInUse", tc.key, err)
		}
	}
}

// (i) Close does not unmap: payloads obtained before Close stay readable
// and intact after it, queries keep answering, and a later open is served
// from the same mapping.
func TestPreparedMappedCloseKeepsMapping(t *testing.T) {
	mustMappedChild(t, "close", map[string]string{"PATH": prepWriteArtifact(t, prepCaseNamed(t, "ngram-filtered")), "CASE": "ngram-filtered"})
}

func mappedChildClose(t *testing.T) {
	c := prepCaseNamed(t, mappedEnv("CASE"))
	path := mappedEnv("PATH")
	st, err := engine.OpenPrepared(path, mappedOpts)
	if err != nil {
		t.Fatal(err)
	}
	l := prepList(t, st)
	want := prepAnswers(t, l, c)
	var slices, copies [][]byte
	for _, pls := range mappedPayloads(t, l, c) {
		for _, p := range pls {
			slices = append(slices, p)
			copies = append(copies, bytes.Clone(p))
		}
	}
	addrs := fmt.Sprint(payloadAddrs(l))
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = nil
	runtime.GC()
	runtime.GC()
	for i := range slices {
		if !bytes.Equal(slices[i], copies[i]) {
			t.Fatalf("payload %d changed after Close", i)
		}
	}
	if got := prepAnswers(t, l, c); !bytes.Equal(got, want) {
		t.Fatal("answers changed after Close")
	}
	again, err := engine.OpenPrepared(path, mappedOpts)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(payloadAddrs(prepList(t, again))) != addrs {
		t.Fatal("reopen after Close is not served from the retained mapping")
	}
	t.Logf("%d payloads intact after Close", len(slices))
}

// 100k sanity numbers for the lane report: LoaderMapped (a child per
// repetition) against LoaderHeap (in-process), LevelFull. Not a benchmark;
// opt-in via KURN_PREPARED_SANITY=1.
func TestPreparedMappedSanity100k(t *testing.T) {
	if os.Getenv("KURN_PREPARED_SANITY") == "" {
		t.Skip("set KURN_PREPARED_SANITY=1")
	}
	const n = 100_000
	es := bench.Generate(42, n)
	for i := range es {
		es[i].Payload = json.RawMessage(fmt.Sprintf(`{"program":"SDN","n":%d}`, i))
	}
	cfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true},
	}
	path := filepath.Join(t.TempDir(), "a.kart")
	if _, err := builder.Write(path, prepListName, cfg, es, builder.Options{}); err != nil {
		t.Fatal(err)
	}
	es = nil
	fi, _ := os.Stat(path)
	t.Logf("artifact %d bytes", fi.Size())
	for rep := 0; rep < 3; rep++ {
		d, heap, total, ver := mappedMeasure(t, path, engine.PreparedOptions{})
		t.Logf("heap   rep %d: %8.1f ms  HeapAlloc Δ %6.1f MiB  TotalAlloc Δ %6.1f MiB  (version %.20s…)", rep, d, heap, total, ver)
	}
	for rep := 0; rep < 3; rep++ {
		out := mustMappedChild(t, "sanity", map[string]string{"PATH": path})
		line := regexp.MustCompile(`MAPPED-SANITY (.*)`).FindStringSubmatch(out)
		if line == nil {
			t.Fatalf("no measurement:\n%s", out)
		}
		t.Logf("mapped rep %d: %s", rep, line[1])
	}
}

func mappedMeasure(t *testing.T, path string, opts engine.PreparedOptions) (ms, heapMiB, totalMiB float64, version string) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	t0 := time.Now()
	s, err := engine.OpenPrepared(path, opts)
	d := time.Since(t0)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	l, _ := s.List(prepListName)
	version = l.Version()
	s.Close()
	runtime.KeepAlive(s)
	return float64(d.Microseconds()) / 1000, float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)) / (1 << 20),
		float64(after.TotalAlloc-before.TotalAlloc) / (1 << 20), version
}

func mappedChildSanity(t *testing.T) {
	d, heap, total, ver := mappedMeasure(t, mappedEnv("PATH"), mappedOpts)
	fmt.Printf("MAPPED-SANITY %8.1f ms  HeapAlloc Δ %6.1f MiB  TotalAlloc Δ %6.1f MiB  (version %.20s…)\n", d, heap, total, ver)
}
