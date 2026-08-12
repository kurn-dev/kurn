package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kurn-dev/kurn/bench"
	"github.com/kurn-dev/kurn/engine"
)

const maxCalibrationThresholds = 32

func cmdCalibrate(args []string) error {
	return cmdCalibrateTo(args, os.Stdout)
}

func cmdCalibrateTo(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
	configPath := fs.String("config", "", "list configuration JSON (ngram mode only)")
	entriesPath := fs.String("entries", "", "entries NDJSON (one {\"id\",\"keys\",[\"payload\"]} per line)")
	corpusPath := fs.String("corpus", "", "labelled query CSV (query,truth_id,category)")
	thresholdsRaw := fs.String("thresholds", "", "comma-separated threshold sweep (2-32 unique values in [0,1])")
	reportPath := fs.String("report-json", "", "also write a privacy-safe kurn-calibration/v1 JSON report to this file (not stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || *entriesPath == "" || *corpusPath == "" || *thresholdsRaw == "" {
		return fmt.Errorf("calibrate: -config, -entries, -corpus, and -thresholds are required")
	}
	if *reportPath == "-" {
		return fmt.Errorf("calibrate: -report-json - is unsupported; use a file path so human stdout stays unambiguous")
	}
	thresholds, err := parseCalibrationThresholds(*thresholdsRaw)
	if err != nil {
		return err
	}
	cfg, err := readCalibrationConfig(*configPath)
	if err != nil {
		return err
	}
	l, err := engine.NewList("calibration", cfg)
	if err != nil {
		return fmt.Errorf("calibrate: config: %w", err)
	}
	if l.Config().Match.Mode != "ngram" {
		return fmt.Errorf("calibrate: config: match mode %q is unsupported; threshold calibration requires ngram mode", l.Config().Match.Mode)
	}

	entries, entriesSHA, err := readEntriesWithSHA256(*entriesPath)
	if err != nil {
		return err
	}
	corpus, corpusSHA, err := readCorpusWithSHA256(*corpusPath)
	if err != nil {
		return err
	}
	if err := validateCalibrationInputs(entries, corpus); err != nil {
		return err
	}
	distinct := distinctKeys(entries)
	if distinct == 0 {
		return fmt.Errorf("calibrate: entries contain no distinct keys")
	}

	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	buildStart := time.Now()
	if err := l.Replace(entries); err != nil {
		return fmt.Errorf("calibrate: build: %w", err)
	}
	buildTime := time.Since(buildStart)
	runtime.GC()
	runtime.ReadMemStats(&m1)
	heapDelta := int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
	if heapDelta < 0 {
		heapDelta = 0
	}

	results, thresholdObservations := runCalibrationSweep(l, corpus, thresholds)
	report := makeCalibrationReport(
		l, entriesSHA, corpusSHA, len(entries), distinct, len(corpus), thresholds,
		results, buildTime, heapDelta, m1.HeapAlloc, thresholdObservations,
	)
	if err := validateCalibrationReport(report); err != nil {
		return fmt.Errorf("calibrate: report: %w", err)
	}
	if *reportPath != "" {
		if err := writeCalibrationReport(*reportPath, report); err != nil {
			return fmt.Errorf("calibrate: report: %w", err)
		}
	}
	return writeHumanCalibrationReport(stdout, report)
}

func parseCalibrationThresholds(raw string) ([]float64, error) {
	parts := strings.Split(raw, ",")
	if len(parts) < 2 || len(parts) > maxCalibrationThresholds {
		return nil, fmt.Errorf("calibrate: -thresholds must contain 2-%d values (got %d)", maxCalibrationThresholds, len(parts))
	}
	thresholds := make([]float64, len(parts))
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("calibrate: -thresholds value %d is empty", i+1)
		}
		value, err := strconv.ParseFloat(part, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return nil, fmt.Errorf("calibrate: -thresholds value %q is not finite in [0,1]", part)
		}
		if value == 0 {
			value = 0 // canonicalize negative zero before identity/JSON emission
		}
		thresholds[i] = value
	}
	sort.Float64s(thresholds)
	for i := 1; i < len(thresholds); i++ {
		if thresholds[i] == thresholds[i-1] {
			return nil, fmt.Errorf("calibrate: -thresholds contains duplicate value %s", strconv.FormatFloat(thresholds[i], 'g', -1, 64))
		}
	}
	return thresholds, nil
}

func readCalibrationConfig(path string) (engine.ListConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return engine.ListConfig{}, fmt.Errorf("calibrate: config %s: %w", path, err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var cfg engine.ListConfig
	if err := dec.Decode(&cfg); err != nil {
		return engine.ListConfig{}, fmt.Errorf("calibrate: config %s: %w", path, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return engine.ListConfig{}, fmt.Errorf("calibrate: config %s: %w", path, err)
	}
	return cfg, nil
}

func validateCalibrationInputs(entries []engine.Entry, corpus []bench.Case) error {
	if len(entries) == 0 {
		return fmt.Errorf("calibrate: entries are empty")
	}
	ids := make(map[string]struct{}, len(entries))
	for i := range entries {
		id := entries[i].ID
		if id == "" {
			return fmt.Errorf("calibrate: entries row %d has an empty id", i+1)
		}
		if _, exists := ids[id]; exists {
			return fmt.Errorf("calibrate: duplicate entry id %q", id)
		}
		ids[id] = struct{}{}
	}
	if len(corpus) == 0 {
		return fmt.Errorf("calibrate: corpus is empty")
	}
	for i := range corpus {
		c := corpus[i]
		if c.TruthID == "" {
			return fmt.Errorf("calibrate: corpus row %d has an empty truth_id", i+2)
		}
		if _, exists := ids[c.TruthID]; !exists {
			return fmt.Errorf("calibrate: corpus row %d truth_id %q is absent from entries", i+2, c.TruthID)
		}
		if c.Category == "" || c.Category == "OVERALL" {
			return fmt.Errorf("calibrate: corpus row %d category %q is empty or reserved", i+2, c.Category)
		}
	}
	return nil
}

type calibrationCaseMeasurement struct {
	category       string
	candidateCount int
	truthRank      int
}

func runCalibrationSweep(l *engine.List, corpus []bench.Case, thresholds []float64) ([]calibrationThresholdResult, []calibrationThresholdObservation) {
	results := make([]calibrationThresholdResult, 0, len(thresholds))
	observations := make([]calibrationThresholdObservation, 0, len(thresholds))
	for _, threshold := range thresholds {
		measurements := make([]calibrationCaseMeasurement, 0, len(corpus))
		latencies := make([]int64, 0, len(corpus))
		optsThreshold := threshold
		if threshold == 0 {
			optsThreshold = -1 // engine sentinel: explicit no floor, not list default
		}
		started := time.Now()
		for _, c := range corpus {
			queryStarted := time.Now()
			candidates := l.Query(c.Query, engine.QueryOpts{Threshold: optsThreshold})
			latencies = append(latencies, time.Since(queryStarted).Microseconds())
			truthRank := 0
			for i := range candidates {
				if candidates[i].EntryID == c.TruthID {
					truthRank = i + 1
					break
				}
			}
			measurements = append(measurements, calibrationCaseMeasurement{
				category: c.Category, candidateCount: len(candidates), truthRank: truthRank,
			})
		}
		total := time.Since(started)
		results = append(results, makeCalibrationThresholdResult(threshold, measurements))
		observations = append(observations, calibrationThresholdObservation{
			Threshold:    threshold,
			LatencyP50US: percentileInt64(latencies, 50),
			LatencyP99US: percentileInt64(latencies, 99),
			QPS:          float64(len(corpus)) / total.Seconds(),
		})
	}
	return results, observations
}

func makeCalibrationThresholdResult(threshold float64, measurements []calibrationCaseMeasurement) calibrationThresholdResult {
	byCategory := make(map[string][]calibrationCaseMeasurement)
	for _, measurement := range measurements {
		byCategory[measurement.category] = append(byCategory[measurement.category], measurement)
	}
	names := orderedCalibrationCategoryNames(byCategory)
	categories := make([]calibrationCategoryResult, 0, len(names))
	for _, name := range names {
		categories = append(categories, summarizeCalibrationMeasurements(name, byCategory[name]))
	}
	return calibrationThresholdResult{
		Threshold:  threshold,
		Categories: categories,
		Overall:    summarizeCalibrationMeasurements("OVERALL", measurements),
	}
}

func orderedCalibrationCategoryNames(byCategory map[string][]calibrationCaseMeasurement) []string {
	seen := make(map[string]bool, len(byCategory))
	names := make([]string, 0, len(byCategory))
	for _, name := range bench.Categories() {
		if len(byCategory[name]) > 0 {
			names = append(names, name)
			seen[name] = true
		}
	}
	var extra []string
	for name := range byCategory {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	return append(names, extra...)
}

func summarizeCalibrationMeasurements(category string, measurements []calibrationCaseMeasurement) calibrationCategoryResult {
	candidateCounts := make([]int, 0, len(measurements))
	truthRanks := make([]int, 0, len(measurements))
	found := 0
	maxCandidates := 0
	for _, measurement := range measurements {
		candidateCounts = append(candidateCounts, measurement.candidateCount)
		if measurement.candidateCount > maxCandidates {
			maxCandidates = measurement.candidateCount
		}
		if measurement.truthRank > 0 {
			found++
			truthRanks = append(truthRanks, measurement.truthRank)
		}
	}
	cases := len(measurements)
	return calibrationCategoryResult{
		Category:          category,
		Cases:             cases,
		Found:             found,
		Misses:            cases - found,
		Recall:            float64(found) / float64(cases),
		CandidateCountP50: percentileInts(candidateCounts, 50),
		CandidateCountP99: percentileInts(candidateCounts, 99),
		CandidateCountMax: maxCandidates,
		TruthRankP50:      percentileInts(truthRanks, 50),
		TruthRankP99:      percentileInts(truthRanks, 99),
	}
}

func percentileInts(values []int, percentile int) int {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]int(nil), values...)
	sort.Ints(ordered)
	return ordered[min(len(ordered)-1, len(ordered)*percentile/100)]
}

func percentileInt64(values []int64, percentile int) int64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]int64(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	return ordered[min(len(ordered)-1, len(ordered)*percentile/100)]
}

func writeHumanCalibrationReport(stdout io.Writer, report calibrationReport) error {
	w := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintf(w, "entries\t%d\n", report.Identity.EntryCount)
	fmt.Fprintf(w, "distinct keys\t%d\n", report.Identity.DistinctKeyCount)
	fmt.Fprintf(w, "corpus cases\t%d\n", report.Identity.CorpusCaseCount)
	fmt.Fprintf(w, "config digest\t%s\n", report.Identity.ResolvedConfig.DigestSHA256)
	fmt.Fprintf(w, "top-K\t%d\t(review burden is within this returned cut)\n", report.Identity.ResolvedConfig.Match.TopK)
	fmt.Fprintf(w, "build time\t%v\n", time.Duration(report.Observations.BuildDurationNS).Round(time.Millisecond))
	fmt.Fprintf(w, "index B/key\t%.1f\t(approx: HeapAlloc delta around Replace, entries excluded)\n", report.Observations.IndexBytesPerDistinctKey)
	fmt.Fprintf(w, "process HeapAlloc\t%.1f MiB\n", float64(report.Observations.ProcessHeapAllocBytes)/(1<<20))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "threshold\tcategory\tcases\tfound\tmisses\trecall\tcandidates p50/p99/max\ttruth rank p50/p99")
	for _, threshold := range report.Results {
		for _, result := range append(append([]calibrationCategoryResult(nil), threshold.Categories...), threshold.Overall) {
			fmt.Fprintf(w, "%g\t%s\t%d\t%d\t%d\t%.3f\t%d/%d/%d\t%d/%d\n",
				threshold.Threshold, result.Category, result.Cases, result.Found, result.Misses, result.Recall,
				result.CandidateCountP50, result.CandidateCountP99, result.CandidateCountMax,
				result.TruthRankP50, result.TruthRankP99)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "threshold\tp50\tp99\tQPS")
	for _, observation := range report.Observations.Thresholds {
		fmt.Fprintf(w, "%g\t%d µs\t%d µs\t%.0f\n", observation.Threshold, observation.LatencyP50US, observation.LatencyP99US, observation.QPS)
	}
	return w.Flush()
}
