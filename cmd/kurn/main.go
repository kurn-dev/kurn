// Command kurn is the kurn dev/bench CLI: benchmarks over synthetic or
// file-loaded corpora, corpus export, and one-off queries against a store dir.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"text/tabwriter"
	"time"

	"github.com/kurn-dev/kurn/bench"
	"github.com/kurn-dev/kurn/engine"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "bench":
		err = cmdBench(os.Args[2:])
	case "gen":
		err = cmdGen(os.Args[2:])
	case "query":
		err = cmdQuery(os.Args[2:])
	case "keygen":
		err = cmdKeygen(os.Args[2:])
	case "ingest":
		err = cmdIngest(os.Args[2:])
	case "build":
		err = cmdBuild(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kurn:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: kurn <command> [flags]

commands:
  bench   build an index over generated or file-loaded entries, report recall/latency/memory
  ingest  dry-run a mapping over a feed: extraction/empty-key report, no build
  build   feed + mapping -> publishable bundle (base.jsonl, base.idx, manifest, delta)
  keygen  generate an API key and its sha256 digest (for kurnd tenants files)
  gen     write generated entries (NDJSON) and query corpus (CSV) for bench -entries/-corpus
  query   run one query against an existing store directory

run "kurn <command> -h" for flags.`)
}

func cmdBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	n := fs.Int("n", 1000000, "number of entries to generate (ignored with -entries)")
	sample := fs.Int("sample", 2000, "entries sampled per category for the query corpus (ignored with -corpus)")
	seed := fs.Int64("seed", 42, "generator seed (ignored with -entries/-corpus)")
	threshold := fs.Float64("threshold", 0.5, "match threshold")
	topk := fs.Int("topk", 100, "top-K candidates")
	mode := fs.String("mode", "ngram", "match mode: ngram | exact")
	entriesPath := fs.String("entries", "", "load entries from this NDJSON file instead of generating (one {\"id\",\"keys\"} per line; pair with -corpus)")
	corpusPath := fs.String("corpus", "", "load the query corpus from this CSV file instead of generating (query,truth_id,category; pair with -entries)")
	fs.Parse(args)
	if *n <= 0 || *sample <= 0 {
		return fmt.Errorf("bench: -n and -sample must be > 0 (got -n %d -sample %d)", *n, *sample)
	}
	if (*entriesPath == "") != (*corpusPath == "") {
		return fmt.Errorf("bench: -entries and -corpus must be given together (the query corpus must match the list content)")
	}

	l, err := engine.NewList("bench", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: *mode, Grams: []int{2, 3}, StripSpaces: true, Threshold: *threshold, TopK: *topk},
	})
	if err != nil {
		return err
	}

	// Entries and query corpus: generated deterministically, or loaded from
	// files — kurn gen's own output round-trips, and any real corpus
	// (downloaded separately) in the same shapes drives the same harness.
	var entries []engine.Entry
	var corpus []bench.Case
	if *entriesPath != "" {
		if entries, err = readEntries(*entriesPath); err != nil {
			return err
		}
		if corpus, err = readCorpus(*corpusPath); err != nil {
			return err
		}
	} else {
		entries = bench.Generate(*seed, *n)
		corpus = bench.Corpus(*seed, entries, *sample)
	}
	distinct := distinctKeys(entries)

	// Memory: snapshot AFTER entries are materialized (and the counting map
	// above is dead) but BEFORE Replace, so B/key covers the index the list
	// builds, not the entries slice itself. HeapAlloc deltas around GC are
	// approximate — allocator slack and anything else the GC moves in the
	// window land in the number too; treat B/key as an estimate.
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	buildStart := time.Now()
	if err := l.Replace(entries); err != nil {
		return err
	}
	buildTime := time.Since(buildStart)
	runtime.GC()
	runtime.ReadMemStats(&m1)
	// Signed delta: HeapAlloc is uint64 and a (theoretical) shrink across
	// Replace would wrap unsigned subtraction to a huge bogus number.
	bPerKey := float64(int64(m1.HeapAlloc)-int64(m0.HeapAlloc)) / float64(distinct)

	rep := bench.Run(l, corpus, engine.QueryOpts{})

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintf(w, "entries\t%d\n", len(entries))
	fmt.Fprintf(w, "distinct keys\t%d\n", distinct)
	fmt.Fprintf(w, "build time\t%v\n", buildTime.Round(time.Millisecond))
	fmt.Fprintf(w, "index B/key\t%.1f\t(approx: HeapAlloc delta around Replace, entries excluded)\n", bPerKey)
	fmt.Fprintf(w, "process HeapAlloc\t%.1f MiB\n", float64(m1.HeapAlloc)/(1<<20))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "category\tcases\tfound\trecall")
	for _, cat := range bench.Categories() {
		cs := rep.ByCategory[cat]
		if cs == nil {
			continue
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%.3f\n", cat, cs.Cases, cs.Found, cs.Recall)
	}
	fmt.Fprintf(w, "OVERALL\t%d\t%d\t%.3f\n", rep.Cases, rep.Found, rep.Recall)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "p50\t%d µs\n", rep.P50Us)
	fmt.Fprintf(w, "p99\t%d µs\n", rep.P99Us)
	fmt.Fprintf(w, "QPS\t%.0f\n", rep.QPS)
	return w.Flush()
}

// readEntries loads one {"id","keys"} JSON entry per line (the shape kurn
// gen writes, and the store's NDJSON entry format).
func readEntries(path string) ([]engine.Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []engine.Entry
	dec := json.NewDecoder(bufio.NewReaderSize(f, 1<<20))
	for {
		var e engine.Entry
		if err := dec.Decode(&e); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("bench: entries %s: %w", path, err)
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("bench: entries %s: empty", path)
	}
	return entries, nil
}

// readCorpus loads the query corpus: CSV with header query,truth_id,category
// (the shape kurn gen writes).
func readCorpus(path string) ([]bench.Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("bench: corpus %s: %w", path, err)
	}
	if len(header) != 3 || header[0] != "query" || header[1] != "truth_id" || header[2] != "category" {
		return nil, fmt.Errorf("bench: corpus %s: header %v, want [query truth_id category]", path, header)
	}
	var corpus []bench.Case
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("bench: corpus %s: %w", path, err)
		}
		corpus = append(corpus, bench.Case{Query: row[0], TruthID: row[1], Category: row[2]})
	}
	if len(corpus) == 0 {
		return nil, fmt.Errorf("bench: corpus %s: no cases", path)
	}
	return corpus, nil
}

func distinctKeys(entries []engine.Entry) int {
	seen := make(map[string]struct{}, len(entries))
	for i := range entries {
		for _, k := range entries[i].Keys {
			seen[k] = struct{}{}
		}
	}
	return len(seen)
}

func cmdGen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	n := fs.Int("n", 1000000, "number of entries to generate")
	seed := fs.Int64("seed", 42, "generator seed")
	sample := fs.Int("sample", 2000, "entries sampled per category for the query corpus")
	out := fs.String("o", "entries.jsonl", "entries output path (NDJSON)")
	corpusPath := fs.String("corpus", "corpus.csv", "corpus output path (CSV)")
	fs.Parse(args)
	if *n <= 0 || *sample <= 0 {
		return fmt.Errorf("gen: -n and -sample must be > 0 (got -n %d -sample %d)", *n, *sample)
	}

	entries := bench.Generate(*seed, *n)
	corpus := bench.Corpus(*seed, entries, *sample)

	if err := writeEntries(*out, entries); err != nil {
		return err
	}
	if err := writeCorpus(*corpusPath, corpus); err != nil {
		return err
	}
	fmt.Printf("wrote %d entries to %s, %d cases to %s\n", len(entries), *out, len(corpus), *corpusPath)
	return nil
}

func writeEntries(path string, entries []engine.Entry) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	enc := json.NewEncoder(bw)
	for i := range entries {
		if err := enc.Encode(entries[i]); err != nil {
			f.Close()
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// writeCorpus emits the query corpus as query,truth_id,category — the same
// shape bench -corpus reads back.
func writeCorpus(path string, corpus []bench.Case) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	cw := csv.NewWriter(f)
	if err := cw.Write([]string{"query", "truth_id", "category"}); err != nil {
		f.Close()
		return err
	}
	for _, c := range corpus {
		if err := cw.Write([]string{c.Query, c.TruthID, c.Category}); err != nil {
			f.Close()
			return err
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// cmdKeygen mints an API key and prints its sha256 digest — the digest
// goes into a kurnd tenants file (key_digests), the key goes to the
// tenant. 32 random bytes, "kurn_" prefix so leaked keys are greppable.
func cmdKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	n := fs.Int("n", 1, "number of keys to generate")
	fs.Parse(args)
	for i := 0; i < *n; i++ {
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return err
		}
		key := "kurn_" + hex.EncodeToString(raw[:])
		digest := sha256.Sum256([]byte(key))
		fmt.Printf("key:    %s\ndigest: %s\n", key, hex.EncodeToString(digest[:]))
	}
	return nil
}

func cmdQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	data := fs.String("data", "", "store directory (required)")
	list := fs.String("list", "", "list name (required)")
	q := fs.String("q", "", "query string (required)")
	threshold := fs.Float64("threshold", 0, "match threshold override (0 = list default)")
	topk := fs.Int("topk", 0, "top-K override (0 = list default)")
	var filters filterFlags
	fs.Var(&filters, "filter", "repeatable name=value: keep only candidates whose payload matches every filter (names must be declared filterable in the list config)")
	filterJSON := fs.String("filter-json", "", "typed filter JSON object (strings, booleans, numbers, or {\"in\":[...]}); mutually exclusive with -filter")
	stats := fs.Bool("stats", false, "with a non-empty -filter or -filter-json: after success, write one filter_stats JSON object to stderr")
	fs.Parse(args)
	if *data == "" || *list == "" || *q == "" {
		fs.Usage()
		return fmt.Errorf("query: -data, -list and -q are required")
	}
	filterJSONSet := false
	fs.Visit(func(f *flag.Flag) { filterJSONSet = filterJSONSet || f.Name == "filter-json" })
	if filterJSONSet && len(filters.m) > 0 {
		return fmt.Errorf("query: -filter-json and -filter are mutually exclusive")
	}
	var typedFilter engine.TypedFilter
	if filterJSONSet {
		var err error
		typedFilter, err = engine.ParseTypedFilter([]byte(*filterJSON))
		if err != nil {
			return fmt.Errorf("query: -filter-json: %w", err)
		}
	}
	filtered := len(filters.m) > 0 || !typedFilter.Empty()
	if *stats && !filtered {
		// Fail closed: silently emitting nothing would read as "the
		// filter evaluated zero candidates".
		return fmt.Errorf("query: -stats requires a non-empty -filter or -filter-json")
	}

	st, err := engine.Open(*data)
	if err != nil {
		return err
	}
	l, ok := st.List(*list)
	if !ok {
		return fmt.Errorf("unknown list %q in %s", *list, *data)
	}
	enc := json.NewEncoder(os.Stdout)
	if filtered {
		// The error-returning filtered path: an undeclared name or a
		// malformed evaluated payload is an error, never a silent
		// unfiltered or empty answer.
		var fq *engine.FilteredQuery
		if filterJSONSet {
			fq, err = l.PrepareTypedFilteredQuery(*q, engine.QueryOpts{Threshold: *threshold, TopK: *topk}, typedFilter)
		} else {
			fq, err = l.PrepareFilteredQuery(*q, engine.QueryOpts{Threshold: *threshold, TopK: *topk}, filters.m)
		}
		if err != nil {
			return err
		}
		cands, _, fst, err := fq.ExecuteStats(context.Background())
		if err != nil {
			return err
		}
		for _, c := range cands {
			if err := enc.Encode(c); err != nil {
				return err
			}
		}
		if *stats {
			// One JSON object on stderr, mirroring the HTTP member shape,
			// only after full success — stdout remains candidate-per-line.
			serr := json.NewEncoder(os.Stderr)
			return serr.Encode(map[string]map[string]map[string]int64{
				"filter_stats": {*list: {"evaluated": fst.Evaluated, "rejected": fst.Rejected}},
			})
		}
		return nil
	}
	for _, c := range l.Query(*q, engine.QueryOpts{Threshold: *threshold, TopK: *topk}) {
		if err := enc.Encode(c); err != nil {
			return err
		}
	}
	return nil
}
