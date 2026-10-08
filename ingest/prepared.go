// Prepared artifact build: the same feed parsing, size checks and duplicate
// policy as the bundle Build, producing one immutable prepared artifact file
// (engine/prepared) instead of a bundle directory. Entries keep the order
// the bundle path would give them, so the artifact's logical base ID equals
// the bundle's base.jsonl SHA-256.
package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/prepared"
	"github.com/kurn-dev/kurn/engine/prepared/builder"
)

// ArtifactOptions tune a prepared artifact build.
type ArtifactOptions struct {
	// Name is the list name stored in the artifact (required; the store's
	// list-name rule applies).
	Name      string
	SkipBad   int
	Source    string // recorded verbatim
	CreatedAt string // recorded verbatim; empty omits (keeps builds deterministic)
}

// ArtifactResult describes a published artifact.
type ArtifactResult struct {
	Path            string
	Name            string
	Mode            string
	Entries         int
	Keys            int64
	Collapsed       int
	BaseID          string // logical base identity (full SHA-256), verified
	ContainerSHA256 string // SHA-256 of the artifact file, verified
	ConfigSHA256    string // resolved configuration digest (List.ConfigDigest)
	InputSHA256     string // SHA-256 of every input byte read
	// ForeignTemps are temporary files from other invocations found next
	// to the destination. They are reported, never removed.
	ForeignTemps []string
}

// BuildArtifact parses the feed through the mapping exactly as Build does
// (validation, record and entry size bounds, byte-identical duplicates
// collapsed, conflicting duplicate IDs refused), builds the list with the
// engine's own code, and publishes a prepared artifact at path. path must
// not exist; publication is absent-only.
func BuildArtifact(m *Mapping, in io.Reader, path string, opts ArtifactOptions) (*ArtifactResult, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if !prepared.ValidListName(opts.Name) {
		return nil, fmt.Errorf("ingest: list name %q is invalid (lowercase letters, digits, '-' and '_', starting with a letter or digit, at most 64 characters)", opts.Name)
	}
	// Fail before a long parse; the publish link is the authoritative check.
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("ingest: %w: %s", prepared.ErrExists, path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	h := sha256.New()
	src := io.TeeReader(in, h)
	entries, collapsed, err := parseEntries(m, src, opts.SkipBad)
	if err != nil {
		return nil, err
	}
	// The receipt covers the whole input, not only what the parser needed.
	if _, err := io.Copy(io.Discard, src); err != nil {
		return nil, err
	}
	inputSum := hex.EncodeToString(h.Sum(nil))

	res, err := builder.Write(path, opts.Name, m.List, entries, builder.Options{
		Source: opts.Source, CreatedAt: opts.CreatedAt, InputSHA256: inputSum, Collapsed: collapsed,
	})
	out := &ArtifactResult{Path: path, Name: opts.Name, Collapsed: collapsed, InputSHA256: inputSum}
	if res != nil {
		out.Name, out.Mode, out.Entries, out.Keys = res.Name, res.Mode, res.Entries, res.Keys
		out.BaseID, out.ContainerSHA256, out.ConfigSHA256 = res.BaseID, res.ContainerSHA256, res.ConfigDigest
		out.ForeignTemps = res.ForeignTemps
	}
	if err != nil {
		return out, err
	}
	return out, nil
}

// parseEntries is Build's parse-and-duplicate policy, restated because
// Build keeps it inline: CONFLICTING duplicate IDs fail the build,
// BYTE-IDENTICAL duplicates collapse (counted), and an empty feed fails.
// The entries come back in first-occurrence order — the order Build hands
// to the store, and therefore the base order. Kept in lockstep with Build
// by TestBuildArtifactMatchesBundle.
//
// One deliberate difference: every entry's ID and keys must be valid UTF-8,
// checked BEFORE the duplicate comparison. That comparison is json.Marshal
// based, and json.Marshal coerces invalid bytes to U+FFFD, so an invalid row
// could otherwise be discarded as a "byte-identical" duplicate of a valid
// one (or of a different invalid one) and the outcome would depend on row
// order. The artifact refuses such bytes and never coerces them (the bundle
// path, ingest.Build, is unchanged and still coerces). The refusal is FATAL
// whatever skipBad says: skip-bad covers records the parser and mapping
// reject, while this rule is the artifact's identity rule, and skipping a
// record here would silently change which records exist.
func parseEntries(m *Mapping, in io.Reader, skipBad int) ([]engine.Entry, int, error) {
	var entries []engine.Entry
	yielded := 0
	seen := map[string]int{}
	dups := 0
	collapsed := 0
	firstDup := ""
	canon := func(e engine.Entry) string {
		b, _ := json.Marshal(e)
		return string(b)
	}
	if _, err := Parse(m, in, Options{SkipBad: skipBad}, func(e engine.Entry) error {
		yielded++
		if err := checkArtifactUTF8(e, yielded); err != nil {
			return err
		}
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
		return nil, 0, err
	}
	if dups > 0 {
		return nil, 0, fmt.Errorf("ingest: %d duplicate record IDs (first: %q) — records need a stable identity; fix the feed or the id path", dups, firstDup)
	}
	if len(entries) == 0 {
		return nil, 0, fmt.Errorf("ingest: feed yielded no entries")
	}
	return entries, collapsed, nil
}

// checkArtifactUTF8 refuses an entry whose ID or any key is not valid UTF-8.
// ordinal is the 1-based position of the entry among those the parser
// yielded (records the mapping filtered out or skip-bad skipped are not
// counted).
func checkArtifactUTF8(e engine.Entry, ordinal int) error {
	if !utf8.ValidString(e.ID) {
		return fmt.Errorf("ingest: entry %d: ID %q is not valid UTF-8; a prepared artifact refuses it rather than coerce it to U+FFFD (-skip-bad does not apply to invalid UTF-8)", ordinal, e.ID)
	}
	for j, k := range e.Keys {
		if !utf8.ValidString(k) {
			return fmt.Errorf("ingest: entry %d (ID %q): key %d %q is not valid UTF-8; a prepared artifact refuses it rather than coerce it to U+FFFD (-skip-bad does not apply to invalid UTF-8)", ordinal, e.ID, j, k)
		}
	}
	return nil
}
