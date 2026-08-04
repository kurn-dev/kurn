// kurn ingest — the dry-run gate as a CLI: parse the first N
// records through mapping + analyzer and report, without building.
// Exit 1 when -max-empty-rate is exceeded, so CI can gate a feed change.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/kurn-dev/kurn/ingest"
)

func cmdIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	mappingPath := fs.String("mapping", "", "mapping JSON path (required)")
	in := fs.String("in", "", "feed path (required; the raw source file)")
	dry := fs.Bool("dry-run", false, "required: report only, never build (full builds are `kurn build`)")
	n := fs.Int("n", 1000, "records to sample")
	samples := fs.Int("samples", 5, "mapped sample entries to print")
	skipBad := fs.Int("skip-bad", 0, "tolerate up to this many bad records")
	maxEmpty := fs.Float64("max-empty-rate", 1.0, "exit 1 when the empty-key-after-analysis rate exceeds this")
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	fs.Parse(args)
	if !*dry {
		return fmt.Errorf("ingest: pass -dry-run (this command only reports; full builds are `kurn build`)")
	}
	if *mappingPath == "" || *in == "" {
		return fmt.Errorf("ingest: -mapping and -in are required")
	}
	raw, err := os.ReadFile(*mappingPath)
	if err != nil {
		return err
	}
	var m ingest.Mapping
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("ingest: mapping: %w", err)
	}
	f, err := os.Open(*in)
	if err != nil {
		return err
	}
	defer f.Close()
	rep, err := ingest.DryRun(&m, f, ingest.Options{SkipBad: *skipBad}, *n, *samples)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintf(w, "sampled\t%d\t(records %d, filtered %d, bad %d)\n", rep.Sampled, rep.Records, rep.Filtered, rep.Bad)
		fmt.Fprintf(w, "ids\t%d distinct\t(%d duplicate within sample)\n", rep.DistinctIDs, rep.DuplicateIDs)
		fmt.Fprintf(w, "keys\t%d raw\t%d analyzed, %d collapsed empty (rate %.3f)\n", rep.RawKeys, rep.AnalyzedKeys, rep.EmptyKeys, rep.EmptyKeyRate)
		fmt.Fprintf(w, "keyless entries\t%d\t(counted in stats, unfindable by any query)\n", rep.KeylessEntries)
		fmt.Fprintf(w, "est. serving memory\t%.1f MiB\t(analyzed keys x mode B/key heuristic, full-feed scale unknown)\n", float64(rep.EstMemoryBytes)/(1<<20))
		fmt.Fprintln(w)
		for i, s := range rep.Samples {
			fmt.Fprintf(w, "sample %d\tid=%s\tkeys=%v\n", i+1, s.ID, s.AnalyzedKeys)
		}
		w.Flush()
	}
	if rep.EmptyKeyRate > *maxEmpty {
		return fmt.Errorf("ingest: empty-key rate %.3f exceeds -max-empty-rate %.3f", rep.EmptyKeyRate, *maxEmpty)
	}
	return nil
}
