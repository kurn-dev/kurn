// Package builder builds prepared artifacts from engine entries with the
// engine's own configuration resolution and index code. It is separate from
// package prepared because the engine's prepared loader imports prepared,
// and this package imports the engine.
package builder

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/prepared"
)

// Options carries build provenance recorded in the build section.
type Options struct {
	Source      string // caller-provided provenance ref, verbatim
	CreatedAt   string // verbatim; empty omits (keeps builds deterministic)
	InputSHA256 string // digest of the source feed, when known
	Collapsed   int    // byte-identical duplicate records collapsed upstream
}

// Result reports a written artifact. BaseID and ContainerSHA256 were
// established by full verification of the exact bytes published.
type Result struct {
	Path            string
	Name            string
	Mode            string
	ExactCompanion  bool
	Entries         int
	Keys            int64
	BaseID          string
	ContainerSHA256 string
	ConfigDigest    string
	AnalyzerDigest  string
	Size            int64
	ForeignTemps    []string
}

// Prepare resolves cfg under name exactly as the engine does, builds the
// base index over entries (in base order) with the engine's own code, and
// returns the artifact contents. Entries must have non-empty, unique IDs
// (the bundle path refuses duplicates too); each payload is stored as the
// exact bytes of its canonical base line. entries is not modified.
func Prepare(name string, cfg engine.ListConfig, entries []engine.Entry, opts Options) (*prepared.Contents, error) {
	if !prepared.ValidListName(name) {
		return nil, fmt.Errorf("prepared: list name %q is invalid (lowercase letters, digits, '-' and '_', starting with a letter or digit, at most 64 characters)", name)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("prepared: no entries")
	}
	l, err := engine.NewList(name, cfg)
	if err != nil {
		return nil, err
	}
	recs := make([]prepared.Record, len(entries))
	canon := make([]engine.Entry, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for i := range entries {
		e := &entries[i]
		if e.ID == "" {
			return nil, fmt.Errorf("prepared: entry %d has an empty ID", i)
		}
		if _, dup := seen[e.ID]; dup {
			return nil, fmt.Errorf("prepared: duplicate entry ID %q", e.ID)
		}
		seen[e.ID] = struct{}{}
		p, err := prepared.CanonicalPayload(e.Payload)
		if err != nil {
			return nil, fmt.Errorf("prepared: entry %q: %w", e.ID, err)
		}
		recs[i] = prepared.Record{ID: e.ID, Keys: e.Keys, Payload: p}
		canon[i] = engine.Entry{ID: e.ID, Keys: e.Keys, Payload: p}
	}
	// The same build the engine store runs for Replace: IDs are unique, so
	// its last-wins dedupe keeps canon in order and ordinal i is entry i.
	if err := l.Replace(canon); err != nil {
		return nil, err
	}
	resolved := l.Config()
	listJSON, err := json.Marshal(resolved)
	if err != nil {
		return nil, err
	}
	an, err := engine.ResolveAnalyzer(resolved.Analyzer)
	if err != nil {
		return nil, err
	}
	dropped, keyless := l.BuildStats()
	return &prepared.Contents{
		Name: name, ListConfig: listJSON,
		AnalyzerDigest: engine.AnalyzerSpecDigest(an), ConfigDigest: l.ConfigDigest(),
		Records: recs, Ngram: l.BaseNgram(), Exact: l.BaseExact(), ExactCompanion: l.BaseExactCompanion(),
		DroppedKeys: dropped, KeylessEntries: keyless,
		Source: opts.Source, CreatedAt: opts.CreatedAt, InputSHA256: opts.InputSHA256, Collapsed: opts.Collapsed,
	}, nil
}

// Encode prepares and encodes an artifact, then fully verifies the bytes
// (structure, digests, recomputed base ID) and checks the configuration
// against the engine before returning them: nothing this package returns
// is rejected by its own reader.
func Encode(name string, cfg engine.ListConfig, entries []engine.Entry, opts Options) ([]byte, *prepared.Verified, error) {
	c, err := Prepare(name, cfg, entries, opts)
	if err != nil {
		return nil, nil, err
	}
	buf, sum, err := prepared.Encode(c)
	if err != nil {
		return nil, nil, err
	}
	v, ver, err := prepared.VerifyBytes(buf, prepared.LevelFull)
	if err != nil {
		return nil, nil, fmt.Errorf("prepared: self-verification of the encoded artifact failed: %w", err)
	}
	if ver.BaseID != sum.BaseID || ver.ContainerSHA256 != sum.ContainerSHA256 {
		return nil, nil, fmt.Errorf("prepared: self-verification disagrees with the encoder")
	}
	if _, err := ListConfig(v); err != nil {
		return nil, nil, fmt.Errorf("prepared: self-verification of the configuration failed: %w", err)
	}
	return buf, ver, nil
}

// Write builds, verifies and publishes an artifact at path, absent-only
// (see prepared.Publish).
func Write(path, name string, cfg engine.ListConfig, entries []engine.Entry, opts Options) (*Result, error) {
	buf, ver, err := Encode(name, cfg, entries, opts)
	if err != nil {
		return nil, err
	}
	pr, err := prepared.PublishBytes(path, buf)
	res := &Result{
		Path: path, Name: ver.Name, Mode: ver.Mode, ExactCompanion: ver.ExactCompanion, Entries: ver.Entries, Keys: ver.Keys,
		BaseID: ver.BaseID, ContainerSHA256: ver.ContainerSHA256,
		ConfigDigest: ver.ConfigDigest, AnalyzerDigest: ver.AnalyzerDigest, Size: ver.Size,
	}
	if pr != nil {
		res.ForeignTemps = pr.ForeignTemps
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

// ListConfig decodes a view's configuration and checks it against the
// engine: it must decode strictly into an engine.ListConfig that NewList
// accepts under the stored name, be exactly the resolved form NewList
// produces (marshaled byte for byte), and reproduce both declared digests.
func ListConfig(v *prepared.View) (engine.ListConfig, error) {
	rc, err := resolveConfig(v)
	if err != nil {
		return engine.ListConfig{}, err
	}
	c := v.Config()
	if rc.configDigest != c.ConfigDigest {
		return engine.ListConfig{}, fmt.Errorf("prepared: config digest %s does not match the configuration (%s)", c.ConfigDigest, rc.configDigest)
	}
	if rc.analyzerDigest != c.AnalyzerDigest {
		return engine.ListConfig{}, fmt.Errorf("prepared: analyzer digest %s does not match the configuration (%s)", c.AnalyzerDigest, rc.analyzerDigest)
	}
	return rc.cfg, nil
}

// resolvedConfig is a view's configuration as the engine resolves it, with
// both digests recomputed by the engine's own functions.
type resolvedConfig struct {
	cfg            engine.ListConfig
	configDigest   string
	analyzerDigest string
}

// resolveConfig is ListConfig without the digest comparison: strict decode,
// NewList under the stored name, the resolved-canonical-form check, and
// the recomputed config and analyzer digests.
func resolveConfig(v *prepared.View) (*resolvedConfig, error) {
	c := v.Config()
	var cfg engine.ListConfig
	dec := json.NewDecoder(bytes.NewReader(c.ListConfig))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("prepared: list config: %w", err)
	}
	l, err := engine.NewList(c.Name, cfg)
	if err != nil {
		return nil, fmt.Errorf("prepared: list config: %w", err)
	}
	resolved := l.Config()
	raw, err := json.Marshal(resolved)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, c.ListConfig) {
		return nil, fmt.Errorf("prepared: list config is not in resolved canonical form")
	}
	an, err := engine.ResolveAnalyzer(resolved.Analyzer)
	if err != nil {
		return nil, err
	}
	return &resolvedConfig{cfg: resolved, configDigest: l.ConfigDigest(), analyzerDigest: engine.AnalyzerSpecDigest(an)}, nil
}
