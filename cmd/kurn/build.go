// kurn build — offline bundle build: feed + mapping →
// publishable bundle (config.json, base.jsonl, base.idx, manifest.json,
// delta.jsonl with -prev). See docs/platform-contract.md §6
// for the publish flow.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kurn-dev/kurn/engine/prepared"
	"github.com/kurn-dev/kurn/ingest"
)

func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	mappingPath := fs.String("mapping", "", "mapping JSON path (required)")
	in := fs.String("in", "", "feed path or - for stdin (required)")
	out := fs.String("out", "", "bundle output directory (required; must not already hold a bundle)")
	prev := fs.String("prev", "", "previous bundle directory (enables delta.jsonl)")
	skipBad := fs.Int("skip-bad", 0, "tolerate up to this many bad records")
	source := fs.String("source", "", "provenance ref recorded in the manifest (e.g. feed URL + publish date)")
	ts := fs.String("ts", "", "created_at recorded in the manifest (RFC3339; empty omits, keeping builds deterministic)")
	artifact := fs.String("artifact", "", "write one prepared artifact FILE instead of a bundle (excludes -out and -prev; the file must not exist)")
	name := fs.String("name", "", "list name stored in the -artifact file (default: the file's base name without its suffix)")
	fs.Parse(args)
	if *artifact != "" || *name != "" {
		return buildArtifact(*mappingPath, *in, *out, *prev, *artifact, *name, *source, *ts, *skipBad)
	}
	if *mappingPath == "" || *in == "" || *out == "" {
		return fmt.Errorf("build: -mapping, -in, and -out are required")
	}
	raw, err := os.ReadFile(*mappingPath)
	if err != nil {
		return err
	}
	var m ingest.Mapping
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("build: mapping: %w", err)
	}
	var feed io.Reader = os.Stdin
	if *in != "-" {
		f, err := os.Open(*in)
		if err != nil {
			return err
		}
		defer f.Close()
		feed = f
	}
	inputHash := sha256.New()
	feed = io.TeeReader(feed, inputHash)
	man, err := ingest.Build(&m, feed, *out, ingest.BuildOptions{
		SkipBad: *skipBad, Source: *source, CreatedAt: *ts, PrevDir: *prev,
	})
	if err != nil {
		return err
	}
	fmt.Printf("input sha256 %x (preserve this receipt with the source ref)\n", inputHash.Sum(nil))
	fmt.Printf("bundle %s: %d entries, %d keys, base %s (mode %s)\n",
		*out, man.Entries, man.Keys, man.VersionID, man.Mode)
	if man.Delta != nil {
		fmt.Printf("delta vs %s: +%d ~%d -%d\n", man.PrevSHA256[:12], man.Delta.Adds, man.Delta.Updates, man.Delta.Deletes)
	}
	return nil
}

// buildArtifact is `kurn build -artifact FILE`: the same feed, mapping and
// duplicate policy as a bundle build, published as one prepared artifact.
func buildArtifact(mappingPath, in, out, prev, artifact, name, source, ts string, skipBad int) error {
	switch {
	case artifact == "":
		return fmt.Errorf("build: -name applies only with -artifact")
	case out != "":
		return fmt.Errorf("build: -artifact and -out are mutually exclusive (-out writes a bundle directory, -artifact one prepared file)")
	case prev != "":
		return fmt.Errorf("build: -prev cannot be combined with -artifact (deltas belong to bundle builds)")
	case mappingPath == "" || in == "":
		return fmt.Errorf("build: -mapping and -in are required")
	}
	if name == "" {
		name = defaultArtifactName(artifact)
	}
	if !prepared.ValidListName(name) {
		return fmt.Errorf("build: list name %q is invalid: use lowercase letters, digits, '-' and '_', starting with a letter or digit, at most 64 characters (set -name)", name)
	}
	raw, err := os.ReadFile(mappingPath)
	if err != nil {
		return err
	}
	var m ingest.Mapping
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("build: mapping: %w", err)
	}
	var feed io.Reader = os.Stdin
	if in != "-" {
		f, err := os.Open(in)
		if err != nil {
			return err
		}
		defer f.Close()
		feed = f
	}
	res, err := ingest.BuildArtifact(&m, feed, artifact, ingest.ArtifactOptions{
		Name: name, SkipBad: skipBad, Source: source, CreatedAt: ts,
	})
	if res != nil {
		for _, t := range res.ForeignTemps {
			fmt.Fprintf(os.Stderr, "note: %s is a temporary file from another build (crashed or still running); it was left alone\n", t)
		}
	}
	if err != nil {
		return err
	}
	fmt.Printf("input sha256 %s (preserve this receipt with the source ref)\n", res.InputSHA256)
	fmt.Printf("artifact %s: list %s, %d entries, %d keys, base %s (mode %s)\n",
		artifact, res.Name, res.Entries, res.Keys, res.BaseID, res.Mode)
	fmt.Printf("container sha256 %s\n", res.ContainerSHA256)
	return nil
}

// defaultArtifactName is the artifact file's base name without its suffix.
func defaultArtifactName(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
