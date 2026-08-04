// kurn build — offline bundle build: feed + mapping →
// publishable bundle (config.json, base.jsonl, base.idx, manifest.json,
// delta.jsonl with -prev). See docs/platform-contract.md §6
// for the publish flow.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/kurn-dev/kurn/ingest"
)

func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	mappingPath := fs.String("mapping", "", "mapping JSON path (required)")
	in := fs.String("in", "", "feed path (required)")
	out := fs.String("out", "", "bundle output directory (required; must not already hold a bundle)")
	prev := fs.String("prev", "", "previous bundle directory (enables delta.jsonl)")
	skipBad := fs.Int("skip-bad", 0, "tolerate up to this many bad records")
	source := fs.String("source", "", "provenance ref recorded in the manifest (e.g. feed URL + publish date)")
	ts := fs.String("ts", "", "created_at recorded in the manifest (RFC3339; empty omits, keeping builds deterministic)")
	fs.Parse(args)
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
	f, err := os.Open(*in)
	if err != nil {
		return err
	}
	defer f.Close()
	man, err := ingest.Build(&m, f, *out, ingest.BuildOptions{
		SkipBad: *skipBad, Source: *source, CreatedAt: *ts, PrevDir: *prev,
	})
	if err != nil {
		return err
	}
	fmt.Printf("bundle %s: %d entries, %d keys, version %s (mode %s)\n",
		*out, man.Entries, man.Keys, man.VersionID, man.Mode)
	if man.Delta != nil {
		fmt.Printf("delta vs %s: +%d ~%d -%d\n", man.PrevSHA256[:12], man.Delta.Adds, man.Delta.Updates, man.Delta.Deletes)
	}
	return nil
}
