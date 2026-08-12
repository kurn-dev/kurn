package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/kurn-dev/kurn/engine"
)

const calibrationReportSchemaV1 = "kurn-calibration/v1"

// calibrationReport keeps decision facts separate from timing and allocator
// observations. Slice order is canonical and every schema-bearing object is
// typed so repeated runs over the same state marshal deterministically after
// observations are removed.
type calibrationReport struct {
	Schema       string                       `json:"schema"`
	Build        evalBuildIdentity            `json:"build"`
	Environment  evalEnvironmentIdentity      `json:"environment"`
	Identity     calibrationIdentity          `json:"identity"`
	Results      []calibrationThresholdResult `json:"results"`
	Observations calibrationObservations      `json:"observations"`
}

type calibrationIdentity struct {
	BenchmarkMode    string                    `json:"benchmark_mode"`
	ResolvedConfig   calibrationResolvedConfig `json:"resolved_config"`
	Input            calibrationInputIdentity  `json:"input"`
	Thresholds       []float64                 `json:"thresholds"`
	EntryCount       int                       `json:"entry_count"`
	DistinctKeyCount int                       `json:"distinct_key_count"`
	CorpusCaseCount  int                       `json:"corpus_case_count"`
}

type calibrationResolvedConfig struct {
	AnalyzerSteps      []string               `json:"analyzer_steps"`
	Match              calibrationMatchConfig `json:"match"`
	FilterableCount    int                    `json:"filterable_count"`
	GoldenProbeCount   int                    `json:"golden_probe_count"`
	OverlayAutoCompact int                    `json:"overlay_auto_compact"`
	DigestSHA256       string                 `json:"digest_sha256"`
}

type calibrationMatchConfig struct {
	Mode             string  `json:"mode"`
	Fallback         string  `json:"fallback"`
	Grams            []int   `json:"grams"`
	StripSpaces      bool    `json:"strip_spaces"`
	DefaultThreshold float64 `json:"default_threshold"`
	TopK             int     `json:"topk"`
}

type calibrationInputIdentity struct {
	Mode          string `json:"mode"`
	EntriesSHA256 string `json:"entries_sha256"`
	CorpusSHA256  string `json:"corpus_sha256"`
}

type calibrationThresholdResult struct {
	Threshold  float64                     `json:"threshold"`
	Categories []calibrationCategoryResult `json:"categories"`
	Overall    calibrationCategoryResult   `json:"overall"`
}

type calibrationCategoryResult struct {
	Category          string  `json:"category"`
	Cases             int     `json:"cases"`
	Found             int     `json:"found"`
	Misses            int     `json:"misses"`
	Recall            float64 `json:"recall"`
	CandidateCountP50 int     `json:"candidate_count_p50"`
	CandidateCountP99 int     `json:"candidate_count_p99"`
	CandidateCountMax int     `json:"candidate_count_max"`
	TruthRankP50      int     `json:"truth_rank_p50"`
	TruthRankP99      int     `json:"truth_rank_p99"`
}

type calibrationObservations struct {
	BuildDurationNS          int64                             `json:"build_duration_ns"`
	IndexHeapDeltaBytes      int64                             `json:"index_heap_delta_bytes"`
	IndexBytesPerDistinctKey float64                           `json:"index_bytes_per_distinct_key"`
	ProcessHeapAllocBytes    uint64                            `json:"process_heap_alloc_bytes"`
	Thresholds               []calibrationThresholdObservation `json:"thresholds"`
}

type calibrationThresholdObservation struct {
	Threshold    float64 `json:"threshold"`
	LatencyP50US int64   `json:"latency_p50_us"`
	LatencyP99US int64   `json:"latency_p99_us"`
	QPS          float64 `json:"qps"`
}

func makeCalibrationReport(
	l *engine.List,
	entriesSHA, corpusSHA string,
	entries, distinct, cases int,
	thresholds []float64,
	results []calibrationThresholdResult,
	buildTime time.Duration,
	heapDelta int64,
	processHeap uint64,
	thresholdObservations []calibrationThresholdObservation,
) calibrationReport {
	cfg := l.Config()
	return calibrationReport{
		Schema:      calibrationReportSchemaV1,
		Build:       currentEvalBuildIdentity(),
		Environment: currentEvalEnvironmentIdentity(),
		Identity: calibrationIdentity{
			BenchmarkMode: "ngram",
			ResolvedConfig: calibrationResolvedConfig{
				AnalyzerSteps: append([]string(nil), cfg.Analyzer.Steps...),
				Match: calibrationMatchConfig{
					Mode: cfg.Match.Mode, Fallback: cfg.Match.Fallback,
					Grams: append([]int(nil), cfg.Match.Grams...), StripSpaces: cfg.Match.StripSpaces,
					DefaultThreshold: cfg.Match.Threshold, TopK: cfg.Match.TopK,
				},
				FilterableCount: len(cfg.Filterable), GoldenProbeCount: len(cfg.Golden),
				OverlayAutoCompact: cfg.OverlayAutoCompact,
				DigestSHA256:       l.ConfigDigest(),
			},
			Input: calibrationInputIdentity{
				Mode: "files", EntriesSHA256: entriesSHA, CorpusSHA256: corpusSHA,
			},
			Thresholds:       append([]float64(nil), thresholds...),
			EntryCount:       entries,
			DistinctKeyCount: distinct,
			CorpusCaseCount:  cases,
		},
		Results: append([]calibrationThresholdResult(nil), results...),
		Observations: calibrationObservations{
			BuildDurationNS:          buildTime.Nanoseconds(),
			IndexHeapDeltaBytes:      heapDelta,
			IndexBytesPerDistinctKey: float64(heapDelta) / float64(distinct),
			ProcessHeapAllocBytes:    processHeap,
			Thresholds:               append([]calibrationThresholdObservation(nil), thresholdObservations...),
		},
	}
}

func validateCalibrationReport(report calibrationReport) error {
	if report.Schema != calibrationReportSchemaV1 {
		return fmt.Errorf("unsupported schema %q", report.Schema)
	}
	if report.Build.ModulePath == "" || report.Build.Version == "" || report.Build.VCSRevision == "" {
		return fmt.Errorf("build identity contains an empty field")
	}
	if report.Build.VCSModified != "true" && report.Build.VCSModified != "false" && report.Build.VCSModified != "unknown" {
		return fmt.Errorf("build vcs_modified %q is invalid", report.Build.VCSModified)
	}
	if report.Environment.GoVersion == "" || report.Environment.GOOS == "" || report.Environment.GOARCH == "" || report.Environment.LogicalCPU <= 0 || report.Environment.GOMAXPROCS <= 0 {
		return fmt.Errorf("environment identity is incomplete")
	}
	id := report.Identity
	if id.BenchmarkMode != "ngram" || id.ResolvedConfig.Match.Mode != "ngram" {
		return fmt.Errorf("calibration requires ngram benchmark and resolved match modes")
	}
	if len(id.ResolvedConfig.AnalyzerSteps) == 0 || len(id.ResolvedConfig.Match.Grams) == 0 {
		return fmt.Errorf("resolved analyzer steps and grams must be non-empty")
	}
	if id.ResolvedConfig.Match.Fallback != "" || !finiteRange(id.ResolvedConfig.Match.DefaultThreshold, 0, 1) || id.ResolvedConfig.Match.TopK <= 0 {
		return fmt.Errorf("resolved match configuration is invalid")
	}
	if id.ResolvedConfig.FilterableCount < 0 || id.ResolvedConfig.GoldenProbeCount < 0 || id.ResolvedConfig.OverlayAutoCompact < 0 || !validSHA256(id.ResolvedConfig.DigestSHA256) {
		return fmt.Errorf("resolved configuration identity is invalid")
	}
	if id.Input.Mode != "files" || !validSHA256(id.Input.EntriesSHA256) || !validSHA256(id.Input.CorpusSHA256) {
		return fmt.Errorf("file input identity is invalid")
	}
	if id.EntryCount <= 0 || id.DistinctKeyCount <= 0 || id.CorpusCaseCount <= 0 {
		return fmt.Errorf("entry, key, and corpus counts must be positive")
	}
	if len(id.Thresholds) < 2 || len(id.Thresholds) > maxCalibrationThresholds {
		return fmt.Errorf("threshold identity must contain 2-%d values", maxCalibrationThresholds)
	}
	for i, threshold := range id.Thresholds {
		if !finiteRange(threshold, 0, 1) || (i > 0 && threshold <= id.Thresholds[i-1]) {
			return fmt.Errorf("threshold identity is not finite, unique, and ascending")
		}
	}
	if len(report.Results) != len(id.Thresholds) {
		return fmt.Errorf("result threshold count does not match identity")
	}
	var categoryNames []string
	for i, result := range report.Results {
		if result.Threshold != id.Thresholds[i] {
			return fmt.Errorf("result threshold %v disagrees with identity %v", result.Threshold, id.Thresholds[i])
		}
		names, err := validateCalibrationThresholdResult(result, id.ResolvedConfig.Match.TopK, id.CorpusCaseCount)
		if err != nil {
			return fmt.Errorf("threshold %v: %w", result.Threshold, err)
		}
		if i == 0 {
			categoryNames = names
		} else if !slices.Equal(names, categoryNames) {
			return fmt.Errorf("category order changes across thresholds")
		}
	}
	o := report.Observations
	if o.BuildDurationNS < 0 || o.IndexHeapDeltaBytes < 0 || !finiteNonnegative(o.IndexBytesPerDistinctKey) {
		return fmt.Errorf("build observations must be finite and non-negative")
	}
	if len(o.Thresholds) != len(id.Thresholds) {
		return fmt.Errorf("observation threshold count does not match identity")
	}
	for i, observation := range o.Thresholds {
		if observation.Threshold != id.Thresholds[i] || observation.LatencyP50US < 0 || observation.LatencyP99US < observation.LatencyP50US || !finiteNonnegative(observation.QPS) {
			return fmt.Errorf("threshold observation %d is invalid", i)
		}
	}
	return nil
}

func validateCalibrationThresholdResult(result calibrationThresholdResult, topK, wantCases int) ([]string, error) {
	if result.Overall.Category != "OVERALL" || result.Overall.Cases != wantCases {
		return nil, fmt.Errorf("overall result must use the reserved category and contain every case")
	}
	if err := validateCalibrationCategoryResult(result.Overall, topK); err != nil {
		return nil, fmt.Errorf("overall result: %w", err)
	}
	if len(result.Categories) == 0 {
		return nil, fmt.Errorf("category results are empty")
	}
	seen := make(map[string]bool, len(result.Categories))
	names := make([]string, 0, len(result.Categories))
	var cases, found, misses int
	for _, category := range result.Categories {
		if category.Category == "" || category.Category == "OVERALL" || seen[category.Category] {
			return nil, fmt.Errorf("category %q is empty, reserved, or repeated", category.Category)
		}
		seen[category.Category] = true
		names = append(names, category.Category)
		if err := validateCalibrationCategoryResult(category, topK); err != nil {
			return nil, fmt.Errorf("category %q: %w", category.Category, err)
		}
		cases += category.Cases
		found += category.Found
		misses += category.Misses
	}
	categorySet := make(map[string][]calibrationCaseMeasurement, len(names))
	for _, name := range names {
		categorySet[name] = []calibrationCaseMeasurement{{category: name}}
	}
	if want := orderedCalibrationCategoryNames(categorySet); !slices.Equal(names, want) {
		return nil, fmt.Errorf("category order %v is not canonical %v", names, want)
	}
	if cases != result.Overall.Cases || found != result.Overall.Found || misses != result.Overall.Misses {
		return nil, fmt.Errorf("category totals do not equal overall totals")
	}
	return names, nil
}

func validateCalibrationCategoryResult(result calibrationCategoryResult, topK int) error {
	if result.Cases <= 0 || result.Found < 0 || result.Found > result.Cases || result.Misses != result.Cases-result.Found {
		return fmt.Errorf("counts are invalid")
	}
	if !finiteRange(result.Recall, 0, 1) || result.Recall != float64(result.Found)/float64(result.Cases) {
		return fmt.Errorf("recall does not equal found/cases")
	}
	if result.CandidateCountP50 < 0 || result.CandidateCountP99 < result.CandidateCountP50 || result.CandidateCountMax < result.CandidateCountP99 || result.CandidateCountMax > topK {
		return fmt.Errorf("candidate-count distribution is invalid or exceeds top-K")
	}
	if result.Found == 0 {
		if result.TruthRankP50 != 0 || result.TruthRankP99 != 0 {
			return fmt.Errorf("zero found cases must have zero truth ranks")
		}
		return nil
	}
	if result.TruthRankP50 <= 0 || result.TruthRankP99 < result.TruthRankP50 || result.TruthRankP99 > result.CandidateCountMax {
		return fmt.Errorf("truth-rank distribution is invalid")
	}
	return nil
}

type calibrationTempFile interface {
	io.Writer
	Name() string
	Sync() error
	Close() error
}

type calibrationReportWriteOps struct {
	CreateTemp func(string, string) (calibrationTempFile, error)
	Rename     func(string, string) error
	Remove     func(string) error
}

var defaultCalibrationReportWriteOps = calibrationReportWriteOps{
	CreateTemp: func(dir, pattern string) (calibrationTempFile, error) { return os.CreateTemp(dir, pattern) },
	Rename:     os.Rename,
	Remove:     os.Remove,
}

func writeCalibrationReport(path string, report calibrationReport) error {
	return writeCalibrationReportWithOps(path, report, defaultCalibrationReportWriteOps)
}

func writeCalibrationReportWithOps(path string, report calibrationReport, ops calibrationReportWriteOps) error {
	if path == "" || path == "-" {
		return fmt.Errorf("report target must be a file path")
	}
	if err := validateCalibrationReport(report); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := ops.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = ops.Remove(tmpName) }
	n, writeErr := tmp.Write(raw)
	if writeErr == nil && n != len(raw) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", writeErr)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := ops.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename temp: %w", err)
	}
	return nil
}
