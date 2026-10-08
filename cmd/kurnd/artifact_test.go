package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/prepared"
	"github.com/kurn-dev/kurn/engine/prepared/builder"
	"github.com/kurn-dev/kurn/server"
)

var testBinOnce sync.Once
var testBin, testBinDir string
var testBinErr error

func TestMain(m *testing.M) {
	code := m.Run()
	if testBinDir != "" {
		_ = os.RemoveAll(testBinDir)
	}
	os.Exit(code)
}

func kurndBinary(t *testing.T) string {
	t.Helper()
	testBinOnce.Do(func() {
		testBinDir, testBinErr = os.MkdirTemp("", "kurnd-l4-test-")
		if testBinErr != nil {
			return
		}
		testBin = filepath.Join(testBinDir, "kurnd")
		if out, err := exec.Command("go", "build", "-o", testBin, ".").CombinedOutput(); err != nil {
			testBinErr = fmt.Errorf("build kurnd: %w: %s", err, out)
		}
	})
	if testBinErr != nil {
		t.Fatal(testBinErr)
	}
	return testBin
}

func TestArtifactFlagIntentAndConflicts(t *testing.T) {
	bin := kurndBinary(t)
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"empty artifact", []string{"-artifact", ""}, "non-empty file path"},
		{"loader without artifact", []string{"-artifact-loader", "heap"}, "requires -artifact"},
		{"verification without artifact", []string{"-artifact-verify", "trusted"}, "requires -artifact"},
		{"missing address", []string{"-artifact", "x"}, "requires an explicit -addr"},
		{"empty address", []string{"-artifact", "x", "-addr", ""}, "requires an explicit -addr"},
		{"data", []string{"-artifact", "x", "-addr", "127.0.0.1:1", "-data", "x"}, "-data cannot be used"},
		{"tenants", []string{"-artifact", "x", "-addr", "127.0.0.1:1", "-tenants-file", "x"}, "-tenants-file cannot be used"},
		{"journal mode", []string{"-artifact", "x", "-addr", "127.0.0.1:1", "-journal-fsync", "none"}, "-journal-fsync cannot be used"},
		{"journal interval", []string{"-artifact", "x", "-addr", "127.0.0.1:1", "-journal-fsync-interval", "1ms"}, "-journal-fsync-interval cannot be used"},
		{"mapped", []string{"-artifact", "x", "-addr", "127.0.0.1:1", "-artifact-loader", "mapped"}, "mapped is not available"},
		{"bad loader", []string{"-artifact", "x", "-addr", "127.0.0.1:1", "-artifact-loader", "mystery"}, "want heap"},
		{"bad verification", []string{"-artifact", "x", "-addr", "127.0.0.1:1", "-artifact-verify", "structure"}, "want full or trusted"},
		{"unknown verification", []string{"-artifact", "x", "-addr", "127.0.0.1:1", "-artifact-verify", "mystery"}, "want full or trusted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, bin, tc.args...).CombinedOutput()
			if err == nil || ctx.Err() != nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("err=%v output=%s, want %q", err, out, tc.want)
			}
		})
	}
}

func preparedStore(t *testing.T) (*engine.Store, string) {
	return preparedStoreAt(t, prepared.LevelFull)
}

func preparedStoreAt(t *testing.T, level prepared.Level) (*engine.Store, string) {
	t.Helper()
	buf, _, err := builder.Encode("people", engine.ListConfig{Analyzer: engine.AnalyzerConfig{Preset: "person-name"}, Match: engine.MatchConfig{Mode: "ngram"}}, []engine.Entry{{ID: "p1", Keys: []string{"Ada Lovelace"}, Payload: json.RawMessage(`{"ok":true}`)}}, builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "people.kurn")
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := engine.OpenPrepared(path, engine.PreparedOptions{Verify: level})
	if err != nil {
		t.Fatal(err)
	}
	return st, path
}

type lifecycleHarness struct {
	addr     <-chan string
	signals  chan os.Signal
	done     chan error
	finished chan struct{}
}

func startLifecycle(t *testing.T, open func(string, engine.PreparedOptions) (*engine.Store, error), handler func(*engine.Store, server.Config) http.Handler, drain time.Duration) *lifecycleHarness {
	return startLifecycleAt(t, prepared.LevelFull, open, handler, drain)
}

func startLifecycleAt(t *testing.T, verify prepared.Level, open func(string, engine.PreparedOptions) (*engine.Store, error), handler func(*engine.Store, server.Config) http.Handler, drain time.Duration) *lifecycleHarness {
	t.Helper()
	addr := make(chan string, 1)
	sigs := make(chan os.Signal, 1)
	if handler == nil {
		handler = func(st *engine.Store, cfg server.Config) http.Handler { return server.NewServer(st, cfg) }
	}
	deps := artifactServeDeps{
		listen: func(network, _ string) (net.Listener, error) {
			ln, err := net.Listen(network, "127.0.0.1:0")
			if err == nil {
				addr <- ln.Addr().String()
			}
			return ln, err
		},
		open: open, handler: handler, drainTimeout: drain,
		signals: func() (<-chan os.Signal, func()) { return sigs, func() {} },
	}
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		done <- serveArtifactWith("ignored", "artifact", verify, nil, 0, time.Second, deps)
		close(finished)
	}()
	h := &lifecycleHarness{addr: addr, signals: sigs, done: done, finished: finished}
	t.Cleanup(func() {
		select {
		case <-finished:
		default:
			sigs <- syscall.SIGTERM
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Error("artifact runner did not stop")
			}
		}
	})
	return h
}

func TestArtifactRequestedVerificationIsPassedAndEnforced(t *testing.T) {
	for _, level := range []prepared.Level{prepared.LevelFull, prepared.LevelTrusted} {
		t.Run(level.String(), func(t *testing.T) {
			_, path := preparedStore(t)
			var got prepared.Level
			h := startLifecycleAt(t, level, func(_ string, opts engine.PreparedOptions) (*engine.Store, error) {
				got = opts.Verify
				return engine.OpenPrepared(path, opts)
			}, nil, time.Second)
			base := h.baseURL(t)
			waitHTTP(t, base+"/artifactz", http.StatusOK)
			if got != level {
				t.Fatalf("open verification=%s, want %s", got, level)
			}
			req, _ := http.NewRequest(http.MethodPost, base+"/v1/query", strings.NewReader(`{"q":"Ada Lovelace","lists":["people"]}`))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.Header.Get("Kurn-Artifact-Verification") != level.String() {
				t.Fatalf("verification header=%q", resp.Header.Get("Kurn-Artifact-Verification"))
			}
			h.signals <- syscall.SIGTERM
			if err := waitDone(t, h.done); err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, mismatch := range []struct {
		request, returned prepared.Level
	}{
		{prepared.LevelTrusted, prepared.LevelFull},
		{prepared.LevelFull, prepared.LevelTrusted},
	} {
		st, _ := preparedStoreAt(t, mismatch.returned)
		h := startLifecycleAt(t, mismatch.request, func(string, engine.PreparedOptions) (*engine.Store, error) {
			return st, nil
		}, nil, time.Second)
		_ = h.baseURL(t)
		if err := waitDone(t, h.done); err == nil || !strings.Contains(err.Error(), "requested "+mismatch.request.String()+" verification") {
			t.Fatalf("request=%s returned=%s mismatch result=%v", mismatch.request, mismatch.returned, err)
		}
	}
}

func TestArtifactOpeningDoesNotClaimRequestedVerification(t *testing.T) {
	release := make(chan struct{})
	h := startLifecycleAt(t, prepared.LevelTrusted, func(string, engine.PreparedOptions) (*engine.Store, error) {
		<-release
		return nil, errors.New("stop")
	}, nil, time.Second)
	base := h.baseURL(t)
	resp, err := http.Get(base + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Kurn-Artifact-Verification"); got != "" {
		t.Fatalf("opening response claimed verification %q", got)
	}
	close(release)
	_ = waitDone(t, h.done)
}

func TestArtifactDrainRetainsEstablishedVerification(t *testing.T) {
	g := newArtifactGate()
	box := &handlerBox{h: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), verification: "trusted"}
	g.ready.Store(box)
	g.state.Store(1)
	box.accepted.Store(true)

	ready := httptest.NewRecorder()
	g.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("ready status=%d", ready.Code)
	}
	g.state.Store(2)
	for _, path := range []string{"/readyz", "/v1/query"} {
		rr := httptest.NewRecorder()
		g.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Kurn-Artifact-Verification") != "trusted" {
			t.Errorf("drain %s: status=%d header=%q", path, rr.Code, rr.Header().Get("Kurn-Artifact-Verification"))
		}
	}
}

func (h *lifecycleHarness) baseURL(t *testing.T) string {
	t.Helper()
	select {
	case addr := <-h.addr:
		return "http://" + addr
	case <-time.After(time.Second):
		t.Fatal("listener did not bind")
		return ""
	}
}

func getStatus(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 300 * time.Millisecond}).Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

func TestArtifactLifecycleOpenInstallAndFailure(t *testing.T) {
	t.Run("opening then install", func(t *testing.T) {
		st, _ := preparedStore(t)
		release := make(chan struct{})
		h := startLifecycle(t, func(string, engine.PreparedOptions) (*engine.Store, error) { <-release; return st, nil }, nil, time.Second)
		base := h.baseURL(t)
		if code, _ := getStatus(t, base+"/livez"); code != 200 {
			t.Fatalf("livez=%d", code)
		}
		if code, body := getStatus(t, base+"/readyz"); code != 503 || !strings.Contains(body, "opening") {
			t.Fatalf("readyz=%d %s", code, body)
		}
		close(release)
		waitHTTP(t, base+"/artifactz", 200)
		h.signals <- syscall.SIGTERM
		if err := waitDone(t, h.done); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("failed open", func(t *testing.T) {
		release := make(chan struct{})
		h := startLifecycle(t, func(string, engine.PreparedOptions) (*engine.Store, error) {
			<-release
			return nil, errors.New("broken")
		}, nil, time.Second)
		base := h.baseURL(t)
		if code, _ := getStatus(t, base+"/v1/lists"); code != 503 {
			t.Fatalf("opening route=%d", code)
		}
		close(release)
		if err := waitDone(t, h.done); err == nil || !strings.Contains(err.Error(), "broken") {
			t.Fatalf("result=%v", err)
		}
	})
}

func TestArtifactLifecycleSignalDuringOpenDoesNotInstall(t *testing.T) {
	st, _ := preparedStore(t)
	release := make(chan struct{})
	openDone := make(chan struct{})
	handled := make(chan struct{}, 1)
	h := startLifecycle(t, func(string, engine.PreparedOptions) (*engine.Store, error) {
		defer close(openDone)
		<-release
		return st, nil
	}, func(*engine.Store, server.Config) http.Handler {
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { handled <- struct{}{} })
	}, time.Second)
	base := h.baseURL(t)
	h.signals <- syscall.SIGTERM
	if err := waitDone(t, h.done); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-openDone:
	case <-time.After(time.Second):
		t.Fatal("late opener was not reaped")
	}
	select {
	case <-handled:
		t.Fatal("late handler installed")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := (&http.Client{Timeout: 100 * time.Millisecond}).Get(base + "/artifactz"); err == nil {
		t.Fatal("listener still admitted requests")
	}
}

func TestArtifactLifecycleDrainAndDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		release bool
	}{{"accepted request finishes", true}, {"deadline is bounded", false}} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := preparedStore(t)
			entered, release := make(chan struct{}), make(chan struct{})
			h := startLifecycle(t, func(string, engine.PreparedOptions) (*engine.Store, error) { return st, nil }, func(*engine.Store, server.Config) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/hold" {
						close(entered)
						<-release
					}
					w.WriteHeader(200)
				})
			}, 80*time.Millisecond)
			base := h.baseURL(t)
			waitHTTP(t, base+"/ready", 200)
			requestDone := make(chan error, 1)
			go func() {
				resp, err := http.Get(base + "/hold")
				if resp != nil {
					resp.Body.Close()
				}
				requestDone <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("request did not enter")
			}
			start := time.Now()
			h.signals <- syscall.SIGTERM
			client := &http.Client{Timeout: 100 * time.Millisecond}
			for deadline := time.Now().Add(300 * time.Millisecond); ; {
				if _, err := client.Get(base + "/later"); err != nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("later request admitted")
				}
				time.Sleep(time.Millisecond)
			}
			if tc.release {
				select {
				case <-h.finished:
					t.Fatal("runner closed the store before the accepted request finished")
				default:
				}
				close(release)
				if err := <-requestDone; err != nil {
					t.Fatal(err)
				}
				if err := waitDone(t, h.done); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := waitDone(t, h.done); err != nil {
					t.Fatal(err)
				}
				if d := time.Since(start); d < 50*time.Millisecond || d > 500*time.Millisecond {
					t.Fatalf("drain=%s", d)
				}
				close(release)
				<-requestDone
			}
		})
	}
}

func waitHTTP(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := (&http.Client{Timeout: 100 * time.Millisecond}).Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not reach %d", url, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func waitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not finish")
		return nil
	}
}

func TestArtifactProcessSignals(t *testing.T) {
	bin := kurndBinary(t)
	_, artifact := preparedStore(t)
	for _, sig := range []os.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			probe, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := probe.Addr().String()
			probe.Close()
			cmd := exec.Command(bin, "-artifact", artifact, "-addr", addr)
			var output strings.Builder
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			finished := false
			defer func() {
				if !finished {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			waitHTTP(t, "http://"+addr+"/artifactz", 200)
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				finished = true
				if err != nil {
					t.Fatalf("exit: %v\n%s", err, output.String())
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("did not exit after %s", sig)
			}
		})
	}
}

func TestPreparedArtifactOnlySubprocessJourney(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	dir := t.TempDir()
	inputs := filepath.Join(dir, "disposable-inputs")
	if err := os.Mkdir(inputs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"feed.ndjson", "ngram-mapping.json", "exact-mapping.json"} {
		b, err := os.ReadFile(filepath.Join(root, "examples", "prepared-artifact", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(inputs, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	kurn := filepath.Join(dir, "kurn")
	if out, err := exec.Command("go", "build", "-o", kurn, filepath.Join(root, "cmd", "kurn")).CombinedOutput(); err != nil {
		t.Fatalf("build kurn: %v: %s", err, out)
	}
	artifacts := map[string]string{}
	for _, mode := range []string{"ngram", "exact"} {
		path := filepath.Join(dir, mode+".kurn")
		name := "people-" + mode
		cmd := exec.Command(kurn, "build", "-mapping", filepath.Join(inputs, mode+"-mapping.json"), "-in", filepath.Join(inputs, "feed.ndjson"), "-artifact", path, "-name", name)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s artifact: %v: %s", mode, err, out)
		}
		if out, err := exec.Command(kurn, "verify", "-semantic", path).CombinedOutput(); err != nil {
			t.Fatalf("verify %s artifact: %v: %s", mode, err, out)
		}
		artifacts[mode] = path
	}
	// Make the exact paths supplied to build unavailable. Runtime receives
	// only the independently placed artifact and kurnd executable.
	if err := os.RemoveAll(inputs); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inputs); !os.IsNotExist(err) {
		t.Fatalf("disposable inputs still accessible: %v", err)
	}

	run := func(mode, verify, body, wantID string) {
		t.Helper()
		probe, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := probe.Addr().String()
		probe.Close()
		cmd := exec.Command(kurndBinary(t), "-artifact", artifacts[mode], "-artifact-verify", verify, "-addr", addr)
		var output strings.Builder
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}()
		waitHTTP(t, "http://"+addr+"/artifactz", http.StatusOK)
		resp, err := http.Post("http://"+addr+"/v1/query", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !bytes.Contains(b, []byte(`"entry_id":"`+wantID+`"`)) || resp.Header.Get("Kurn-Artifact-Verification") != verify {
			t.Fatalf("%s/%s query: status=%d header=%q body=%s\n%s", mode, verify, resp.StatusCode, resp.Header.Get("Kurn-Artifact-Verification"), b, output.String())
		}
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("stop %s: %v\n%s", mode, err, output.String())
		}
		if !strings.Contains(output.String(), "verification: "+verify) {
			t.Fatalf("startup log did not name effective policy %q:\n%s", verify, output.String())
		}
	}

	run("exact", "full", `{"q":"Ada Lovelace","lists":["people-exact"]}`, "p1")
	run("ngram", "trusted", `{"q":"lovelce ada","lists":["people-ngram"]}`, "p1")
	run("ngram", "trusted", `{"q":"lovelce ada","lists":["people-ngram"],"filter":{"country":"GB"}}`, "p1")
	// A fresh process restarts from the same artifact, still without the
	// original feed or mappings.
	run("ngram", "full", `{"q":"hopper grace","lists":["people-ngram"]}`, "p2")
}
