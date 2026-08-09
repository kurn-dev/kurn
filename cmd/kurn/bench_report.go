package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"time"

	"github.com/kurn-dev/kurn/bench"
	"github.com/kurn-dev/kurn/engine"
)

const (
	evalReportSchemaV1       = "kurn-eval/v1"
	evalSyntheticGeneratorV1 = "bench.Generate+bench.Corpus/v1"
)

// evalReport is intentionally map-free. Struct field order is the serialized
// order, and category results are normalized to a stable slice before
// marshaling. Observations are separated from reproducible run identity so a
// consumer cannot accidentally treat timing or allocator noise as identity.
type evalReport struct {
	Schema       string                  `json:"schema"`
	Build        evalBuildIdentity       `json:"build"`
	Environment  evalEnvironmentIdentity `json:"environment"`
	Identity     evalRunIdentity         `json:"identity"`
	Results      evalResults             `json:"results"`
	Observations evalObservations        `json:"observations"`
}

type evalBuildIdentity struct {
	ModulePath  string `json:"module_path"`
	Version     string `json:"version"`
	VCSRevision string `json:"vcs_revision"`
	VCSModified string `json:"vcs_modified"`
}

type evalEnvironmentIdentity struct {
	GoVersion  string `json:"go_version"`
	GOOS       string `json:"goos"`
	GOARCH     string `json:"goarch"`
	LogicalCPU int    `json:"logical_cpu"`
	GOMAXPROCS int    `json:"gomaxprocs"`
}

type evalRunIdentity struct {
	BenchmarkMode    string                     `json:"benchmark_mode"`
	ResolvedConfig   evalResolvedConfigIdentity `json:"resolved_config"`
	Input            evalInputIdentity          `json:"input"`
	EntryCount       int                        `json:"entry_count"`
	DistinctKeyCount int                        `json:"distinct_key_count"`
}

type evalResolvedConfigIdentity struct {
	AnalyzerSteps []string                `json:"analyzer_steps"`
	Match         evalMatchConfigIdentity `json:"match"`
	DigestSHA256  string                  `json:"digest_sha256"`
}

type evalMatchConfigIdentity struct {
	Mode        string  `json:"mode"`
	Fallback    string  `json:"fallback"`
	Grams       []int   `json:"grams"`
	StripSpaces bool    `json:"strip_spaces"`
	Threshold   float64 `json:"threshold"`
	TopK        int     `json:"topk"`
}

type evalInputIdentity struct {
	Mode      string                      `json:"mode"`
	Files     *evalFileInputIdentity      `json:"files,omitempty"`
	Synthetic *evalSyntheticInputIdentity `json:"synthetic,omitempty"`
}

type evalFileInputIdentity struct {
	EntriesSHA256 string `json:"entries_sha256"`
	CorpusSHA256  string `json:"corpus_sha256"`
}

type evalSyntheticInputIdentity struct {
	Generator string `json:"generator"`
	Seed      int64  `json:"seed"`
	Entries   int    `json:"entries"`
	Sample    int    `json:"sample"`
}

type evalResults struct {
	Categories []evalCategoryResult `json:"categories"`
	Overall    evalCategoryResult   `json:"overall"`
}

type evalCategoryResult struct {
	Category string  `json:"category"`
	Cases    int     `json:"cases"`
	Found    int     `json:"found"`
	Recall   float64 `json:"recall"`
}

type evalObservations struct {
	BuildDurationNS          int64   `json:"build_duration_ns"`
	IndexHeapDeltaBytes      int64   `json:"index_heap_delta_bytes"`
	IndexBytesPerDistinctKey float64 `json:"index_bytes_per_distinct_key"`
	ProcessHeapAllocBytes    uint64  `json:"process_heap_alloc_bytes"`
	LatencyP50US             int64   `json:"latency_p50_us"`
	LatencyP99US             int64   `json:"latency_p99_us"`
	QPS                      float64 `json:"qps"`
}

func makeEvalReport(l *engine.List, input evalInputIdentity, entries, distinct int, buildTime time.Duration, heapDelta int64, processHeap uint64, rep bench.Report) evalReport {
	if heapDelta < 0 {
		// A negative delta is allocator/GC noise, not negative memory. The
		// human report remains the historical signed estimate; the portable
		// artifact clamps that noise to zero so its fixed-unit observation is
		// always finite and non-negative.
		heapDelta = 0
	}
	cfg := l.Config()
	categories := orderedEvalCategories(rep)
	return evalReport{
		Schema:      evalReportSchemaV1,
		Build:       currentEvalBuildIdentity(),
		Environment: currentEvalEnvironmentIdentity(),
		Identity: evalRunIdentity{
			BenchmarkMode: cfg.Match.Mode,
			ResolvedConfig: evalResolvedConfigIdentity{
				AnalyzerSteps: append([]string{}, cfg.Analyzer.Steps...),
				Match: evalMatchConfigIdentity{
					Mode:        cfg.Match.Mode,
					Fallback:    cfg.Match.Fallback,
					Grams:       append([]int{}, cfg.Match.Grams...),
					StripSpaces: cfg.Match.StripSpaces,
					Threshold:   cfg.Match.Threshold,
					TopK:        cfg.Match.TopK,
				},
				DigestSHA256: l.ConfigDigest(),
			},
			Input:            input,
			EntryCount:       entries,
			DistinctKeyCount: distinct,
		},
		Results: evalResults{
			Categories: categories,
			Overall: evalCategoryResult{
				Category: "OVERALL",
				Cases:    rep.Cases,
				Found:    rep.Found,
				Recall:   rep.Recall,
			},
		},
		Observations: evalObservations{
			BuildDurationNS:          buildTime.Nanoseconds(),
			IndexHeapDeltaBytes:      heapDelta,
			IndexBytesPerDistinctKey: float64(heapDelta) / float64(distinct),
			ProcessHeapAllocBytes:    processHeap,
			LatencyP50US:             rep.P50Us,
			LatencyP99US:             rep.P99Us,
			QPS:                      rep.QPS,
		},
	}
}

func orderedEvalCategories(rep bench.Report) []evalCategoryResult {
	seen := make(map[string]bool, len(rep.ByCategory))
	names := make([]string, 0, len(rep.ByCategory))
	for _, name := range bench.Categories() {
		if rep.ByCategory[name] != nil {
			names = append(names, name)
			seen[name] = true
		}
	}
	var extra []string
	for name := range rep.ByCategory {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	names = append(names, extra...)
	out := make([]evalCategoryResult, 0, len(names))
	for _, name := range names {
		cs := rep.ByCategory[name]
		out = append(out, evalCategoryResult{Category: name, Cases: cs.Cases, Found: cs.Found, Recall: cs.Recall})
	}
	return out
}

func currentEvalBuildIdentity() evalBuildIdentity {
	out := evalBuildIdentity{
		ModulePath:  "unknown",
		Version:     "unknown",
		VCSRevision: "unknown",
		VCSModified: "unknown",
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return out
	}
	if info.Main.Path != "" {
		out.ModulePath = info.Main.Path
	}
	if info.Main.Version != "" {
		out.Version = info.Main.Version
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if setting.Value != "" {
				out.VCSRevision = setting.Value
			}
		case "vcs.modified":
			if modified, err := strconv.ParseBool(setting.Value); err == nil {
				out.VCSModified = strconv.FormatBool(modified)
			}
		}
	}
	return out
}

func currentEvalEnvironmentIdentity() evalEnvironmentIdentity {
	return evalEnvironmentIdentity{
		GoVersion:  runtime.Version(),
		GOOS:       runtime.GOOS,
		GOARCH:     runtime.GOARCH,
		LogicalCPU: runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
	}
}

func validateEvalReport(report evalReport) error {
	if report.Schema != evalReportSchemaV1 {
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
	if report.Identity.BenchmarkMode != report.Identity.ResolvedConfig.Match.Mode {
		return fmt.Errorf("benchmark mode %q disagrees with resolved match mode %q", report.Identity.BenchmarkMode, report.Identity.ResolvedConfig.Match.Mode)
	}
	if report.Identity.BenchmarkMode != "ngram" && report.Identity.BenchmarkMode != "exact" {
		return fmt.Errorf("benchmark mode %q is unsupported", report.Identity.BenchmarkMode)
	}
	if len(report.Identity.ResolvedConfig.AnalyzerSteps) == 0 {
		return fmt.Errorf("resolved analyzer steps are empty")
	}
	if !validSHA256(report.Identity.ResolvedConfig.DigestSHA256) {
		return fmt.Errorf("resolved config digest is not lowercase SHA-256")
	}
	if !finiteRange(report.Identity.ResolvedConfig.Match.Threshold, 0, 1) || report.Identity.ResolvedConfig.Match.TopK < 0 {
		return fmt.Errorf("resolved match configuration is invalid")
	}
	if report.Identity.EntryCount <= 0 || report.Identity.DistinctKeyCount <= 0 {
		return fmt.Errorf("entry and distinct-key counts must be positive")
	}
	switch report.Identity.Input.Mode {
	case "files":
		if report.Identity.Input.Files == nil || report.Identity.Input.Synthetic != nil {
			return fmt.Errorf("file input identity must contain only files")
		}
		if !validSHA256(report.Identity.Input.Files.EntriesSHA256) || !validSHA256(report.Identity.Input.Files.CorpusSHA256) {
			return fmt.Errorf("file input identity is not lowercase SHA-256")
		}
	case "synthetic":
		if report.Identity.Input.Synthetic == nil || report.Identity.Input.Files != nil {
			return fmt.Errorf("synthetic input identity must contain only synthetic")
		}
		s := report.Identity.Input.Synthetic
		if s.Generator != evalSyntheticGeneratorV1 || s.Entries <= 0 || s.Sample <= 0 {
			return fmt.Errorf("synthetic input identity is incomplete")
		}
	default:
		return fmt.Errorf("input mode %q is unsupported", report.Identity.Input.Mode)
	}
	if err := validateEvalResult(report.Results.Overall); err != nil {
		return fmt.Errorf("overall result: %w", err)
	}
	if report.Results.Overall.Category != "OVERALL" || report.Results.Overall.Cases == 0 {
		return fmt.Errorf("overall result must use the reserved category and contain cases")
	}
	if len(report.Results.Categories) == 0 {
		return fmt.Errorf("category results are empty")
	}
	seen := make(map[string]bool, len(report.Results.Categories))
	var cases, found int
	for _, result := range report.Results.Categories {
		if result.Category == "" || result.Category == "OVERALL" || seen[result.Category] {
			return fmt.Errorf("category %q is empty, reserved, or repeated", result.Category)
		}
		seen[result.Category] = true
		if err := validateEvalResult(result); err != nil {
			return fmt.Errorf("category %q: %w", result.Category, err)
		}
		cases += result.Cases
		found += result.Found
	}
	if cases != report.Results.Overall.Cases || found != report.Results.Overall.Found {
		return fmt.Errorf("category totals do not equal overall totals")
	}
	o := report.Observations
	if o.BuildDurationNS < 0 || o.IndexHeapDeltaBytes < 0 || o.LatencyP50US < 0 || o.LatencyP99US < 0 || !finiteNonnegative(o.IndexBytesPerDistinctKey) || !finiteNonnegative(o.QPS) {
		return fmt.Errorf("observations must be finite and non-negative")
	}
	return nil
}

func validateEvalResult(result evalCategoryResult) error {
	if result.Cases < 0 || result.Found < 0 || result.Found > result.Cases || !finiteRange(result.Recall, 0, 1) {
		return fmt.Errorf("counts or recall are invalid")
	}
	if result.Cases == 0 && result.Recall != 0 {
		return fmt.Errorf("zero cases must have zero recall")
	}
	if result.Cases > 0 && result.Recall != float64(result.Found)/float64(result.Cases) {
		return fmt.Errorf("recall does not equal found/cases")
	}
	return nil
}

func finiteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func finiteRange(value, low, high float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= low && value <= high
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	raw, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(raw) == value
}

type evalTempFile interface {
	io.Writer
	Name() string
	Sync() error
	Close() error
}

type evalReportWriteOps struct {
	CreateTemp func(string, string) (evalTempFile, error)
	Rename     func(string, string) error
	Remove     func(string) error
}

var defaultEvalReportWriteOps = evalReportWriteOps{
	CreateTemp: func(dir, pattern string) (evalTempFile, error) { return os.CreateTemp(dir, pattern) },
	Rename:     os.Rename,
	Remove:     os.Remove,
}

func writeEvalReport(path string, report evalReport) error {
	return writeEvalReportWithOps(path, report, defaultEvalReportWriteOps)
}

func writeEvalReportWithOps(path string, report evalReport, ops evalReportWriteOps) error {
	if path == "" || path == "-" {
		return fmt.Errorf("report target must be a file path")
	}
	if err := validateEvalReport(report); err != nil {
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
