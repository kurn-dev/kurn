// kurnd serves a kurn data directory over HTTP.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	data := flag.String("data", "./data", "data directory")
	journalFsync := flag.String("journal-fsync", "none",
		`journal-append durability: "none" (survives process crash, not power loss), "every" (fsync per mutation), or "interval" (group commit)`)
	fsyncInterval := flag.Duration("journal-fsync-interval", 2*time.Millisecond,
		"group-commit window for -journal-fsync=interval")
	queryMemMB := flag.Int64("query-mem-budget-mb", 1024,
		"scratch-memory budget for in-flight queries in MiB (~4B x list ordinals per query); 0 disables admission control")
	queueTimeout := flag.Duration("query-queue-timeout", 2*time.Second,
		"max wait for query admission before 503")
	apiKeysFile := flag.String("api-keys-file", "",
		"file with one API key per line (# comments, blanks skipped); when set, /v1/* requires a key — probes and metrics stay open. Rotation = edit + restart")
	tenantsFile := flag.String("tenants-file", "",
		"multi-tenant registry (JSON: tenant -> {key_digests, quotas}); each tenant's lists live under <data>/tenants/<id> and are reached only via its keys. SIGHUP re-reads the file (add/remove tenants and keys without restart). Mutually exclusive with -api-keys-file")
	flag.Parse()
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
			for name := range opened {
				if _, ok := specs[name]; !ok {
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
