// Offline bundle build: stream the mapping into an engine
// store — the SAME code path the server uses, so the bundle is exactly
// what a node would have written itself — and emit the publishable
// bundle: config.json + base.jsonl + base.idx + manifest.json (+
// delta.jsonl against a previous bundle). The manifest's sha256 of
// base.jsonl IS the node's content-addressed version identity: its
// 12-hex prefix equals the version stamp a node reports after loading
// the bundle, so publish-time and query-time identity are one number.
package ingest

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kurn-dev/kurn/engine"
)

// Manifest describes a built bundle.
type Manifest struct {
	V         int    `json:"v"` // manifest format version (1)
	SHA256    string `json:"sha256"`
	Collapsed int    `json:"collapsed,omitempty"` // byte-identical duplicate records collapsed
	VersionID string `json:"version_id"`          // 12-hex prefix == node version stamp base
	Analyzer  string `json:"analyzer"`            // analyzer spec digest (also inside base.idx)
	Mode      string `json:"mode"`
	Entries   int    `json:"entries"`
	Keys      int64  `json:"keys"`
	Source    string `json:"source,omitempty"`     // caller-provided provenance ref
	CreatedAt string `json:"created_at,omitempty"` // caller-provided (determinism: never wall clock)

	PrevSHA256 string      `json:"prev_sha256,omitempty"`
	Delta      *DeltaStats `json:"delta,omitempty"`
}

// DeltaStats summarize delta.jsonl.
type DeltaStats struct {
	Adds    int `json:"adds"`
	Updates int `json:"updates"`
	Deletes int `json:"deletes"`
}

// BuildOptions tune a build run.
type BuildOptions struct {
	SkipBad   int
	Source    string // recorded in the manifest verbatim
	CreatedAt string // recorded verbatim; empty omits (keeps builds deterministic)
	PrevDir   string // previous bundle dir; enables delta.jsonl
}

// Build parses the feed through the mapping, builds the list, and writes
// the bundle into outDir. outDir must not already contain a bundle.
func Build(m *Mapping, in io.Reader, outDir string, opts BuildOptions) (*Manifest, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(outDir, "base.jsonl")); err == nil {
		return nil, fmt.Errorf("ingest: %s already contains a bundle", outDir)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	// Parse everything (the offline builder holds the corpus — Replace
	// needs the full slice anyway) and enforce the stable-ID requirement:
	// CONFLICTING duplicate IDs are a build error, loudly, not a silent
	// last-wins. BYTE-IDENTICAL duplicates collapse (counted in the
	// manifest): government feeds ship literal duplicate rows (LEIE
	// carries ~26), and two records identical in every mapped byte are
	// one fact — collapsing loses nothing, while an ID naming two
	// DIFFERENT facts still fails the build.
	var entries []engine.Entry
	seen := map[string]int{}
	dups := 0
	collapsed := 0
	firstDup := ""
	canon := func(e engine.Entry) string {
		b, _ := json.Marshal(e)
		return string(b)
	}
	if _, err := Parse(m, in, Options{SkipBad: opts.SkipBad}, func(e engine.Entry) error {
		if prev, ok := seen[e.ID]; ok {
			if canon(entries[prev]) == canon(e) {
				collapsed++
				return nil
			}
			dups++
			if firstDup == "" {
				firstDup = e.ID
			}
		}
		seen[e.ID] = len(entries)
		entries = append(entries, e)
		return nil
	}); err != nil {
		return nil, err
	}
	if dups > 0 {
		return nil, fmt.Errorf("ingest: %d duplicate record IDs (first: %q) — records need a stable identity; fix the feed or the id path", dups, firstDup)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("ingest: feed yielded no entries")
	}

	// Build through a temp store INSIDE outDir (same filesystem — the
	// moves below stay atomic renames).
	tmp, err := os.MkdirTemp(outDir, ".build-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	st, err := engine.Open(tmp)
	if err != nil {
		return nil, err
	}
	if _, err := st.CreateList("bundle", m.List); err != nil {
		return nil, err
	}
	if err := st.Replace("bundle", entries); err != nil {
		return nil, err
	}
	l, _ := st.List("bundle")
	keyCount := l.KeyCount()

	// EVERY output — delta and manifest included — is assembled inside the
	// staging dir before anything reaches outDir. An earlier version
	// renamed the store files out first and built the delta in place, so a
	// delta failure left config.json/base.jsonl/base.idx behind, and the
	// next Build refused the directory as "already contains a bundle": the
	// failure blocked exactly the retry it invited.
	stage := filepath.Join(tmp, "bundle")
	sum, err := fileSHA256(filepath.Join(stage, "base.jsonl"))
	if err != nil {
		return nil, err
	}
	man := &Manifest{
		V: 1, SHA256: sum, VersionID: sum[:12], Collapsed: collapsed,
		Mode: m.List.Match.Mode, Entries: len(entries), Keys: keyCount,
		Source: opts.Source, CreatedAt: opts.CreatedAt,
	}
	an, err := engine.ResolveAnalyzer(m.List.Analyzer)
	if err != nil {
		return nil, err
	}
	man.Analyzer = engine.AnalyzerSpecDigest(an)

	files := []string{"config.json", "base.idx", "manifest.json"}
	if opts.PrevDir != "" {
		prevSum, err := fileSHA256(filepath.Join(opts.PrevDir, "base.jsonl"))
		if err != nil {
			return nil, fmt.Errorf("ingest: prev bundle: %w", err)
		}
		man.PrevSHA256 = prevSum
		ds, err := writeDelta(filepath.Join(opts.PrevDir, "base.jsonl"),
			filepath.Join(stage, "base.jsonl"), filepath.Join(stage, "delta.jsonl"))
		if err != nil {
			return nil, err
		}
		man.Delta = ds
		files = append(files, "delta.jsonl")
	}

	raw, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), append(raw, '\n'), 0o644); err != nil {
		return nil, err
	}

	// Publish. Renames within one filesystem; base.jsonl goes LAST because
	// it is the file the already-contains-a-bundle guard checks, so until
	// it lands the directory still reads as retryable, and a crash between
	// renames leaves leftovers a retry simply overwrites — overwrites, so a
	// leftover the CURRENT build does not produce must be removed instead:
	// a delta-enabled attempt that failed leaves delta.jsonl, and a
	// no-delta retry would otherwise publish a manifest with no delta
	// stats beside a stale delta file that convention-driven consumers
	// would still read.
	if opts.PrevDir == "" {
		if err := os.Remove(filepath.Join(outDir, "delta.jsonl")); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("ingest: removing stale delta: %w", err)
		}
	}
	for _, f := range append(files, "base.jsonl") {
		if err := os.Rename(filepath.Join(stage, f), filepath.Join(outDir, f)); err != nil {
			return nil, fmt.Errorf("ingest: publishing bundle: %w", err)
		}
	}
	return man, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// deltaRec is one delta.jsonl line. Adds and updates carry the full entry
// (downstream re-check percolation needs the keys); deletes carry the ID only.
type deltaRec struct {
	Op    string          `json:"op"` // add | update | delete
	ID    string          `json:"id"`
	Entry json.RawMessage `json:"entry,omitempty"`
}

// writeDelta diffs prev vs next base.jsonl by entry ID. Memory: a 16-byte
// line hash per previous entry — both files are the engine's canonical
// json.Marshal output, so byte equality is entry equality.
func writeDelta(prevPath, nextPath, outPath string) (*DeltaStats, error) {
	prev := map[string][16]byte{}
	if err := eachBaseLine(prevPath, func(id string, line []byte) error {
		s := sha256.Sum256(line)
		var h [16]byte
		copy(h[:], s[:16])
		prev[id] = h
		return nil
	}); err != nil {
		return nil, err
	}

	out, err := os.Create(outPath)
	if err != nil {
		return nil, err
	}
	// A half-written delta must not survive the error that produced it: the
	// next build refuses an output dir that "already contains a bundle", so
	// the leftover blocks exactly the retry this failure invites.
	ok := false
	defer func() {
		out.Close()
		if !ok {
			os.Remove(outPath)
		}
	}()
	w := bufio.NewWriterSize(out, 1<<20)
	enc := json.NewEncoder(w)
	ds := &DeltaStats{}
	seen := map[string]struct{}{}
	if err := eachBaseLine(nextPath, func(id string, line []byte) error {
		seen[id] = struct{}{}
		ph, existed := prev[id]
		if existed {
			s := sha256.Sum256(line)
			if [16]byte(s[:16]) == ph {
				return nil // unchanged
			}
			ds.Updates++
			return enc.Encode(deltaRec{Op: "update", ID: id, Entry: json.RawMessage(line)})
		}
		ds.Adds++
		return enc.Encode(deltaRec{Op: "add", ID: id, Entry: json.RawMessage(line)})
	}); err != nil {
		out.Close()
		return nil, err
	}
	// Deletes: previous IDs absent from the new base, in prev-file order
	// for determinism (map iteration is random — re-walk the file).
	if err := eachBaseLine(prevPath, func(id string, line []byte) error {
		if _, ok := seen[id]; ok {
			return nil
		}
		ds.Deletes++
		return enc.Encode(deltaRec{Op: "delete", ID: id})
	}); err != nil {
		out.Close()
		return nil, err
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return nil, err
	}
	if err := out.Close(); err != nil {
		return nil, err
	}
	ok = true
	return ds, nil
}

// eachBaseLine streams base.jsonl lines with their entry IDs.
func eachBaseLine(path string, fn func(id string, line []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), MaxRecordBytes+4096)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var idOnly struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(line, &idOnly); err != nil {
			return fmt.Errorf("ingest: %s: %w", filepath.Base(path), err)
		}
		cp := append([]byte(nil), line...)
		if err := fn(idOnly.ID, cp); err != nil {
			return err
		}
	}
	return sc.Err()
}
