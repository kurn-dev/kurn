package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/prepared"
	"github.com/kurn-dev/kurn/engine/prepared/builder"
)

func verifyEntries() []engine.Entry {
	return []engine.Entry{
		{ID: "1", Keys: []string{"Anna Smith"}, Payload: []byte(`{"program":"SDN"}`)},
		{ID: "2", Keys: []string{"Bo Chan", "   "}, Payload: []byte(`{"program":"SDGT"}`)},
		{ID: "3", Keys: []string{}},
		{ID: "4", Keys: nil},
		{ID: "5", Keys: []string{"Carla Diaz", "Diaz, Carla"}},
		{ID: "6", Keys: []string{"Anna Smithe"}},
		{ID: "7", Keys: []string{"  "}}, // keyless: its only key analyzes to nothing
	}
}

// verifyFixtures writes the artifacts the CLI tests verify.
func verifyFixtures(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	cfgs := map[string]engine.ListConfig{
		"ngram": {Analyzer: engine.AnalyzerConfig{Preset: "person-name"}, Match: engine.MatchConfig{Mode: "ngram"}},
		"exact": {Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}}, Match: engine.MatchConfig{Mode: "exact"}},
	}
	out := map[string]string{}
	for name, cfg := range cfgs {
		p := filepath.Join(dir, name+".kurn")
		if _, err := builder.Write(p, name, cfg, verifyEntries(), builder.Options{Source: "cli-test"}); err != nil {
			t.Fatal(err)
		}
		out[name] = p
	}
	good, err := os.ReadFile(out["ngram"])
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, b []byte) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		out[name] = p
	}
	// A rehashed semantic defect: a wrong declared keyless_entries.
	c, err := builder.Prepare("ngram", cfgs["ngram"], verifyEntries(), builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c.KeylessEntries++
	defect, _, err := prepared.Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	write("defect", defect)
	// A stale digest: one byte of the build section's source changed in
	// place (still canonical JSON, so structurally valid), no rehash.
	stale := bytes.Clone(good)
	i := bytes.Index(stale, []byte(`"cli-test"`))
	if i < 0 {
		t.Fatal("no source in the build section")
	}
	stale[i+1] = 'C'
	write("stale", stale)
	badMagic := bytes.Clone(good)
	copy(badMagic, "NOTKURN!")
	write("badmagic", badMagic)
	write("truncated", good[:len(good)/2])
	out["missing"] = filepath.Join(dir, "does-not-exist.kurn")
	out["dir"] = dir
	return out
}

func runVerify(args ...string) (code int, stdout, stderr string) {
	var o, e bytes.Buffer
	code = verifyExitTo(verifyTo(args, &o, &e), &e)
	return code, o.String(), e.String()
}

func TestVerifyExitCodes(t *testing.T) {
	f := verifyFixtures(t)
	for _, tc := range []struct {
		name string
		args []string
		code int
		want string // in stdout on success, in stderr on failure
	}{
		{"clean ngram", []string{f["ngram"]}, 0, `at level "full"`},
		{"clean exact", []string{f["exact"]}, 0, `at level "full"`},
		{"clean ngram semantic", []string{"-semantic", f["ngram"]}, 0, `at level "full + semantic"`},
		{"clean exact semantic", []string{"-semantic", f["exact"]}, 0, `index (exact)     ok`},
		{"defect without semantic", []string{f["defect"]}, 0, `at level "full"`},
		{"defect semantic", []string{"-semantic", f["defect"]}, 3, "keyless_entries: declared 2, the records give 1"},
		{"stale digest", []string{f["stale"]}, 3, "digest mismatch"},
		{"stale digest semantic", []string{"-semantic", f["stale"]}, 3, "digest mismatch"},
		{"bad magic", []string{f["badmagic"]}, 4, "bad magic"},
		{"truncated", []string{"-semantic", f["truncated"]}, 4, "malformed"},
		{"no file", nil, 2, "missing FILE"},
		{"unknown flag", []string{"-bogus", f["ngram"]}, 2, "flag provided but not defined"},
		{"extra args", []string{f["ngram"], f["exact"]}, 2, "expected one FILE"},
		{"flag after file", []string{f["ngram"], "-semantic"}, 2, "expected one FILE"},
		{"nonexistent", []string{f["missing"]}, 1, "no such file"},
		{"nonexistent semantic", []string{"-semantic", f["missing"]}, 1, "no such file"},
		{"directory", []string{f["dir"]}, 1, "not a regular file"},
		{"directory semantic", []string{"-semantic", f["dir"]}, 1, "not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runVerify(tc.args...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.code, stdout, stderr)
			}
			if code == 0 {
				if !strings.Contains(stdout, tc.want) || !strings.Contains(stdout, "does not authenticate") {
					t.Fatalf("stdout lacks %q or the authentication note:\n%s", tc.want, stdout)
				}
				return
			}
			if stdout != "" {
				t.Fatalf("stdout after a failure:\n%s", stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr lacks %q:\n%s", tc.want, stderr)
			}
		})
	}
}

func TestVerifyOutput(t *testing.T) {
	f := verifyFixtures(t)
	_, plain, _ := runVerify(f["ngram"])
	ver, err := prepared.Verify(f["ngram"], prepared.LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"list              ngram", "mode              ngram", "format            KURNART1 1.1", "entries           7", "base id           " + ver.BaseID, "container sha256  " + ver.ContainerSHA256, "declared, not recomputed at this level", "config digest     " + ver.ConfigDigest, "analyzer digest   " + ver.AnalyzerDigest} {
		if !strings.Contains(plain, want) {
			t.Errorf("plain output lacks %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "semantic checks") {
		t.Errorf("plain output claims semantic checks:\n%s", plain)
	}
	_, sem, _ := runVerify("-semantic", f["ngram"])
	for _, want := range []string{"semantic checks", "config digest     ok", "analyzer digest   ok", "index (ngram)     ok", "dropped_keys      ok  declared 2, recomputed 2", "keyless_entries   ok  declared 1, recomputed 1", "declared, not verifiable from the artifact:", `source            "cli-test"`, "collapsed         0"} {
		if !strings.Contains(sem, want) {
			t.Errorf("semantic output lacks %q:\n%s", want, sem)
		}
	}
	// A semantic failure prints the report details to stderr.
	code, stdout, stderr := runVerify("-semantic", f["defect"])
	if code != 3 || stdout != "" || !strings.Contains(stderr, "SEMANTIC VERIFICATION FAILED") || !strings.Contains(stderr, "keyless_entries   MISMATCH") || !strings.Contains(stderr, "does not authenticate") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

func TestVerifyExitCodeMapping(t *testing.T) {
	for err, want := range map[error]int{
		nil:                             0,
		&verifyUsageError{msg: "x"}:     2,
		prepared.ErrIntegrity:           3,
		prepared.ErrMalformed:           4,
		prepared.ErrIncompatible:        4,
		prepared.ErrUnaligned:           4,
		errors.New("permission denied"): 1,
		&os.PathError{Op: "open", Err: os.ErrNotExist}: 1,
	} {
		if got := verifyExitCode(err); got != want {
			t.Errorf("verifyExitCode(%v) = %d, want %d", err, got, want)
		}
	}
}

// TestVerifyMainDispatch runs the real main in a child process, so the
// exit codes are the ones kurn verify actually exits with.
func TestVerifyMainDispatch(t *testing.T) {
	if args, ok := os.LookupEnv("KURN_VERIFY_MAIN_ARGS"); ok {
		os.Args = append([]string{"kurn", "verify"}, strings.Split(args, "\x1f")...)
		if args == "" {
			os.Args = os.Args[:2]
		}
		main()
		os.Exit(0)
	}
	f := verifyFixtures(t)
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"-semantic", f["exact"]}, 0},
		{[]string{"-semantic", f["defect"]}, 3},
		{[]string{f["stale"]}, 3},
		{[]string{f["badmagic"]}, 4},
		{nil, 2},
		{[]string{f["missing"]}, 1},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestVerifyMainDispatch$")
		cmd.Env = append(os.Environ(), "KURN_VERIFY_MAIN_ARGS="+strings.Join(tc.args, "\x1f"))
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		err := cmd.Run()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != tc.code {
			t.Errorf("kurn verify %q exited %d, want %d", tc.args, code, tc.code)
		}
		if code != 0 && strings.Contains(stdout.String(), "verified ") {
			t.Errorf("kurn verify %q printed a success line after failing:\n%s", tc.args, stdout.String())
		}
	}
}
