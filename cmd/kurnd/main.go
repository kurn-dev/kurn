// kurnd serves a kurn data directory over HTTP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/prepared"
	"github.com/kurn-dev/kurn/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	data := flag.String("data", "./data", "data directory")
	artifact := flag.String("artifact", "", "serve one prepared artifact read-only (requires explicit -addr; management endpoints on this listener are unauthenticated)")
	artifactLoader := flag.String("artifact-loader", "heap", `prepared-artifact loader: "heap" only; "mapped" is available through the Go library, not this CLI`)
	artifactVerify := flag.String("artifact-verify", "full", `prepared-artifact verification policy: "full" (default) or explicit operator-trusted "trusted"`)
	journalFsync := flag.String("journal-fsync", "none",
		`journal-append durability: "none" (survives process crash, not power loss), "every" (fsync per mutation), or "interval" (group commit)`)
	fsyncInterval := flag.Duration("journal-fsync-interval", 2*time.Millisecond,
		"group-commit window for -journal-fsync=interval")
	queryMemMB := flag.Int64("query-mem-budget-mb", 1024,
		"scratch-memory budget for in-flight queries in MiB (each query is charged a conservative ceiling: ~8B x list ordinals plus a bounded per-hit term); 0 disables admission control")
	queueTimeout := flag.Duration("query-queue-timeout", 2*time.Second,
		"max wait for query admission before 503")
	apiKeysFile := flag.String("api-keys-file", "",
		"file with one API key per line (# comments, blanks skipped); when set, /v1/* requires a key — probes and metrics stay open. Rotation = edit + restart")
	tenantsFile := flag.String("tenants-file", "",
		"multi-tenant registry (JSON: tenant -> {key_digests, quotas}); each tenant's lists live under <data>/tenants/<id> and are reached only via its keys. SIGHUP re-reads the file (add/remove tenants and keys without restart). Mutually exclusive with -api-keys-file")
	flag.Parse()
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	artifactMode := set["artifact"]
	if set["artifact-loader"] && !artifactMode {
		log.Fatalf("-artifact-loader requires -artifact")
	}
	if set["artifact-verify"] && !artifactMode {
		log.Fatalf("-artifact-verify requires -artifact")
	}
	if artifactMode {
		if *artifact == "" {
			log.Fatalf("-artifact requires a non-empty file path")
		}
		if !set["addr"] || *addr == "" {
			log.Fatalf("-artifact requires an explicit -addr (for local serving, for example -addr 127.0.0.1:8080)")
		}
		for _, name := range []string{"data", "tenants-file", "journal-fsync", "journal-fsync-interval"} {
			if set[name] {
				log.Fatalf("-%s cannot be used with -artifact", name)
			}
		}
		if *tenantsFile != "" {
			log.Fatalf("-tenants-file cannot be used with -artifact")
		}
		if *artifactLoader != "heap" {
			if *artifactLoader == "mapped" {
				log.Fatalf("-artifact-loader=mapped is not available in kurnd; use the Go library for mapped loading")
			}
			log.Fatalf("invalid -artifact-loader %q (want heap)", *artifactLoader)
		}
		var verify prepared.Level
		switch *artifactVerify {
		case "full":
			verify = prepared.LevelFull
		case "trusted":
			verify = prepared.LevelTrusted
		default:
			log.Fatalf("invalid -artifact-verify %q (want full or trusted)", *artifactVerify)
		}
		if err := serveArtifact(*addr, *artifact, verify, apiKeysFromFile(*apiKeysFile), *queryMemMB, *queueTimeout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *tenantsFile != "" && *apiKeysFile != "" {
		log.Fatalf("-tenants-file and -api-keys-file are mutually exclusive (the registry carries per-tenant keys)")
	}
	var apiKeys []string
	if *apiKeysFile != "" {
		raw, err := os.ReadFile(*apiKeysFile)
		if err != nil {
			log.Fatalf("read -api-keys-file: %v", err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				apiKeys = append(apiKeys, line)
			}
		}
		if len(apiKeys) == 0 {
			log.Fatalf("-api-keys-file %s contains no keys", *apiKeysFile)
		}
		log.Printf("kurn: API-key auth ON (%d keys); /healthz /livez /readyz /metrics stay open", len(apiKeys))
	}
	var fsyncMode engine.FsyncMode
	switch *journalFsync {
	case "none":
		fsyncMode = engine.FsyncNone
	case "every":
		fsyncMode = engine.FsyncEvery
	case "interval":
		fsyncMode = engine.FsyncInterval
	default:
		log.Fatalf("invalid -journal-fsync %q (want none, every, or interval)", *journalFsync)
	}

	start := time.Now()
	st, err := engine.Open(*data)
	if err != nil {
		log.Fatalf("open %s: %v", *data, err)
	}
	st.JournalFsync = fsyncMode
	st.JournalFsyncInterval = *fsyncInterval
	for _, q := range st.Quarantined {
		log.Printf("kurn: list %s: journal could not be applied (%v) — quarantined to %s; list opened at base state, journaled operations NOT live until repaired by hand", q.List, q.Err, q.Path)
	}
	for _, sk := range st.Skipped {
		log.Printf("kurn: list dir %s: skipped, NOT serving (%v) — repair with a fresh PUT /v1/lists/%s or remove the directory", sk.List, sk.Err, sk.List)
	}
	log.Printf("kurn: loaded %s in %s", *data, time.Since(start).Round(time.Millisecond))
	web := server.NewServer(st, server.Config{
		QueryMemBudget:    *queryMemMB << 20,
		QueryQueueTimeout: *queueTimeout,
		APIKeys:           apiKeys,
	})
	if *tenantsFile != "" {
		// Multi-tenant: one store per tenant under <data>/tenants/<id>.
		// The root store keeps serving tenants marked shared_reads (the
		// free-node shape: platform-published lists under per-tenant
		// keys/limits); with no such tenants it sits harmlessly empty.
		// opened persists across reloads so a SIGHUP reuses live stores.
		opened := map[string]*engine.Store{}
		apply := func() error {
			raw, err := os.ReadFile(*tenantsFile)
			if err != nil {
				return err
			}
			specs, err := server.ParseTenants(raw)
			if err != nil {
				return err
			}
			rts := make(map[string]server.TenantRuntime, len(specs))
			for name, spec := range specs {
				if spec.SharedReads {
					// Shared-reads tenants use the root store (the free-
					// node shape: platform-published lists, per-tenant
					// keys/limits); no per-tenant dir is opened or created.
					rts[name] = server.TenantRuntime{Spec: spec}
					continue
				}
				tst := opened[name]
				if tst == nil {
					tst, err = engine.Open(filepath.Join(*data, "tenants", name))
					if err != nil {
						return fmt.Errorf("tenant %s: %w", name, err)
					}
					tst.JournalFsync = fsyncMode
					tst.JournalFsyncInterval = *fsyncInterval
					for _, q := range tst.Quarantined {
						log.Printf("kurn: tenant %s: list %s: journal quarantined (%v) -> %s", name, q.List, q.Err, q.Path)
					}
					for _, sk := range tst.Skipped {
						log.Printf("kurn: tenant %s: list dir %s skipped (%v)", name, sk.List, sk.Err)
					}
					opened[name] = tst
				}
				rts[name] = server.TenantRuntime{Spec: spec, Store: tst}
			}
			if err := web.SetTenants(rts); err != nil {
				return err
			}
			// Release stores of tenants the new registry dropped: routing is
			// gone (in-flight requests finish on the old registry's live
			// objects), so the reference is the only thing pinning the
			// indexes in RAM. Data stays on disk.
			//
			// Close first, and not just to be tidy: dropping the reference
			// alone leaves the store's auto-compaction running against a
			// directory that a re-add — remove tenant, SIGHUP, add it back,
			// SIGHUP — reopens under a second *engine.Store. The abandoned
			// compactor then folds ITS stale view and truncates the journal,
			// destroying operations the new store already acknowledged.
			// Close returns only once nothing of the old store will write
			// there again, so the reopen is safe by the time it can happen.
			for name := range opened {
				if _, ok := specs[name]; !ok {
					if cerr := opened[name].Close(); cerr != nil {
						log.Printf("kurn: tenant %s: closing dropped store: %v", name, cerr)
					}
					delete(opened, name)
				}
			}
			return nil
		}
		if err := apply(); err != nil {
			log.Fatalf("tenants: %v", err)
		}
		log.Printf("kurn: multi-tenant mode ON (%s); SIGHUP re-reads the registry", *tenantsFile)
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		go func() {
			for range hup {
				if err := apply(); err != nil {
					log.Printf("kurn: tenants reload FAILED, keeping previous registry: %v", err)
				} else {
					log.Printf("kurn: tenants registry reloaded")
				}
			}
		}()
	}
	srv := &http.Server{
		Handler: web,
		// ReadHeaderTimeout only, no ReadTimeout: NDJSON upsert bodies
		// stream and may legitimately take longer than any fixed body
		// deadline. IdleTimeout is safe — it applies between requests.
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	// Bind before logging readiness: "listening on" is a readiness signal
	// (bench scripts grep for it), and with -addr :0 it reports the real port.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	log.Printf("kurn: listening on %s", ln.Addr())
	// No graceful shutdown: the engine is crash-safe via journal replay on Open.
	log.Fatal(srv.Serve(ln))
}

func apiKeysFromFile(path string) []string {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read -api-keys-file: %v", err)
	}
	var keys []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			keys = append(keys, line)
		}
	}
	if len(keys) == 0 {
		log.Fatalf("-api-keys-file %s contains no keys", path)
	}
	return keys
}

type artifactGate struct {
	state atomic.Int32 // 0 opening, 1 ready, 2 draining
	ready atomic.Pointer[handlerBox]
}

type handlerBox struct {
	h            http.Handler
	verification string
	accepted     atomic.Bool
}

func newArtifactGate() *artifactGate {
	return &artifactGate{}
}

func (g *artifactGate) phase() string {
	switch g.state.Load() {
	case 1:
		return "ready"
	case 2:
		return "draining"
	default:
		return "opening"
	}
}

func (g *artifactGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if web := g.ready.Load(); web != nil && web.accepted.Load() {
		if g.state.Load() == 1 {
			web.h.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/readyz" || strings.HasPrefix(r.URL.Path, "/v1/") {
			w.Header().Set("Kurn-Artifact-Verification", web.verification)
		}
	}
	phase := g.phase()
	if r.Method == http.MethodGet && r.URL.Path == "/livez" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"ok"}`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, "{\"phase\":%q}\n", phase)
}

type artifactServeDeps struct {
	listen       func(string, string) (net.Listener, error)
	open         func(string, engine.PreparedOptions) (*engine.Store, error)
	handler      func(*engine.Store, server.Config) http.Handler
	signals      func() (<-chan os.Signal, func())
	drainTimeout time.Duration
}

func productionArtifactDeps() artifactServeDeps {
	return artifactServeDeps{
		listen: net.Listen,
		open:   engine.OpenPrepared,
		handler: func(st *engine.Store, cfg server.Config) http.Handler {
			return server.NewServer(st, cfg)
		},
		signals: func() (<-chan os.Signal, func()) {
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
			return ch, func() { signal.Stop(ch) }
		},
		drainTimeout: 10 * time.Second,
	}
}

func serveArtifact(addr, path string, verify prepared.Level, apiKeys []string, queryMemMB int64, queueTimeout time.Duration) error {
	return serveArtifactWith(addr, path, verify, apiKeys, queryMemMB, queueTimeout, productionArtifactDeps())
}

func serveArtifactWith(addr, path string, verify prepared.Level, apiKeys []string, queryMemMB int64, queueTimeout time.Duration, deps artifactServeDeps) error {
	// Register termination before binding or starting the opener: there is no
	// startup interval in which a termination request can be lost.
	sig, stopSignals := deps.signals()
	defer stopSignals()
	watcherDone := make(chan struct{})
	defer close(watcherDone)
	ln, err := deps.listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	gate := newArtifactGate()
	httpSrv := &http.Server{Handler: gate, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	log.Printf("kurn: artifact listener on %s; management endpoints /livez /readyz /artifactz are unauthenticated", ln.Addr())

	type opened struct {
		st  *engine.Store
		err error
	}
	openedCh := make(chan opened, 1)
	go func() {
		st, err := deps.open(path, engine.PreparedOptions{Loader: engine.LoaderHeap, Verify: verify})
		openedCh <- opened{st, err}
	}()
	terminating := make(chan struct{})
	go func() {
		select {
		case <-sig:
			gate.state.Store(2)
			close(terminating)
		case <-watcherDone:
		}
	}()
	select {
	case got := <-openedCh:
		if got.err != nil {
			_ = httpSrv.Close()
			return fmt.Errorf("open artifact %s: %w", path, got.err)
		}
		info := got.st.Prepared()
		if info == nil || info.Level != verify || !info.IdentitiesRecomputed {
			_ = got.st.Close()
			_ = httpSrv.Close()
			return fmt.Errorf("open artifact %s: requested %s verification and recomputed configuration identities were not established", path, verify)
		}
		web := deps.handler(got.st, server.Config{QueryMemBudget: queryMemMB << 20, QueryQueueTimeout: queueTimeout, APIKeys: apiKeys})
		box := &handlerBox{h: web, verification: info.Level.String()}
		gate.ready.Store(box)
		if !gate.state.CompareAndSwap(0, 1) {
			gate.ready.CompareAndSwap(box, nil)
			_ = got.st.Close()
			_ = httpSrv.Close()
			return nil
		}
		box.accepted.Store(true)
		if verify == prepared.LevelFull {
			log.Printf("kurn: artifact %s ready (verification: full; content digests and base identity recomputed): list %s, %d entries, container %s", path, info.Name, info.Entries, info.ContainerSHA256)
		} else {
			log.Printf("kurn: artifact %s ready (verification: trusted; content digests and base identity were NOT recomputed; operator-asserted): list %s, %d entries", path, info.Name, info.Entries)
		}
		select {
		case <-terminating:
			ctx, cancel := context.WithTimeout(context.Background(), deps.drainTimeout)
			err = httpSrv.Shutdown(ctx)
			cancel()
			if err != nil {
				log.Printf("kurn: artifact drain deadline reached: %v", err)
				_ = httpSrv.Close()
			}
			if err := got.st.Close(); err != nil {
				return fmt.Errorf("close artifact store: %w", err)
			}
			return nil
		case err := <-serveErr:
			_ = got.st.Close()
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("serve artifact: %w", err)
			}
			return nil
		}
	case <-terminating:
		_ = httpSrv.Close()
		// OpenPrepared has no cancellation hook. Reap a late result for
		// in-process callers; it is never installed after draining begins.
		go func() {
			got := <-openedCh
			if got.st != nil {
				_ = got.st.Close()
			}
		}()
		// The process owns the opening heap. Returning from main guarantees a
		// late successful open cannot install a handler or escape this process.
		return nil
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve artifact: %w", err)
		}
		return nil
	}
}
