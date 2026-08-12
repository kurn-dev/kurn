package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kurn-dev/kurn/bench"
	"github.com/kurn-dev/kurn/engine"
)

func TestParseCalibrationThresholdsCanonicalAndStrict(t *testing.T) {
	got, err := parseCalibrationThresholds("0.75, -0, 0.45")
	if err != nil {
		t.Fatal(err)
	}
	if want := []float64{0, 0.45, 0.75}; !reflect.DeepEqual(got, want) {
		t.Fatalf("thresholds = %v, want %v", got, want)
	}
	for _, raw := range []string{
		"0.5", "", "0.5,0.5", "-0.1,0.5", "0.5,1.1", "NaN,0.5", "Inf,0.5", "0.5,",
		strings.Repeat("0.1,", maxCalibrationThresholds) + "0.2",
	} {
		if _, err := parseCalibrationThresholds(raw); err == nil {
			t.Errorf("invalid threshold set %q accepted", raw)
		}
	}
}

func TestCalibrationMetricsIncludeZeroCountsAndFoundRanksOnly(t *testing.T) {
	measurements := []calibrationCaseMeasurement{
		{category: "ZETA", candidateCount: 0, truthRank: 0},
		{category: "EXACT", candidateCount: 4, truthRank: 2},
		{category: "EXACT", candidateCount: 1, truthRank: 1},
		{category: "ALPHA", candidateCount: 2, truthRank: 0},
	}
	result := makeCalibrationThresholdResult(0.5, measurements)
	if got, want := []string{result.Categories[0].Category, result.Categories[1].Category, result.Categories[2].Category}, []string{"EXACT", "ALPHA", "ZETA"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("category order = %v, want %v", got, want)
	}
	o := result.Overall
	if o.Cases != 4 || o.Found != 2 || o.Misses != 2 || o.Recall != 0.5 {
		t.Fatalf("overall counts = %+v", o)
	}
	// Sorted candidate counts [0 1 2 4]: upper median and floor-index p99.
	if o.CandidateCountP50 != 2 || o.CandidateCountP99 != 4 || o.CandidateCountMax != 4 {
		t.Fatalf("candidate distribution = %+v", o)
	}
	// Misses do not enter rank percentiles: ranks are [1 2].
	if o.TruthRankP50 != 2 || o.TruthRankP99 != 2 {
		t.Fatalf("truth-rank distribution = %+v", o)
	}
	if zeta := result.Categories[2]; zeta.TruthRankP50 != 0 || zeta.TruthRankP99 != 0 {
		t.Fatalf("all-miss category has nonzero ranks: %+v", zeta)
	}
}

func TestCalibrationReviewBurdenStopsAtReturnedTopK(t *testing.T) {
	l, err := engine.NewList("topk", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "ngram", TopK: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{
		{ID: "a", Keys: []string{"shared key"}},
		{ID: "b", Keys: []string{"shared key"}},
		{ID: "c", Keys: []string{"shared key"}},
	}); err != nil {
		t.Fatal(err)
	}
	results, _ := runCalibrationSweep(l, []bench.Case{{Query: "shared key", TruthID: "c", Category: "EXACT"}}, []float64{0.5})
	o := results[0].Overall
	if o.CandidateCountMax != 2 || o.Found != 0 || o.Misses != 1 || o.TruthRankP50 != 0 {
		t.Fatalf("top-K-scoped burden = %+v, want two returned candidates and cut truth recorded as a miss", o)
	}
}

func TestCalibrateCommandReportDeterminismPrivacyAndNoFloor(t *testing.T) {
	dir := t.TempDir()
	configBytes := []byte(`{
  "analyzer": {"steps": ["lowercase", "trim"]},
  "match": {"mode": "ngram", "grams": [2, 3], "strip_spaces": true, "threshold": 0.6, "topk": 2},
  "filterable": [{"name": "private-scope-name", "path": "private.scope.path"}]
}`)
	entriesBytes := []byte("{\"id\":\"private-id-1\",\"keys\":[\"alpha one\"],\"payload\":{\"private\":\"payload-one\"}}\n" +
		"{\"id\":\"private-id-2\",\"keys\":[\"alpha two\"]}\n" +
		"{\"id\":\"private-id-3\",\"keys\":[\"omega three\"]}\n")
	corpusBytes := []byte("query,truth_id,category\n" +
		"alpha one,private-id-1,EXACT\n" +
		"alpha one,private-id-2,PARTIAL\n" +
		"omega three,private-id-3,EXACT\n" +
		"private-query-needle,private-id-3,MISS\n")
	configPath := filepath.Join(dir, "private-config-name.json")
	entriesPath := filepath.Join(dir, "private-entries-name.jsonl")
	corpusPath := filepath.Join(dir, "private-corpus-name.csv")
	reportPath := filepath.Join(dir, "private-report-name.json")
	mustWriteTestFile(t, configPath, configBytes)
	mustWriteTestFile(t, entriesPath, entriesBytes)
	mustWriteTestFile(t, corpusPath, corpusBytes)

	args := []string{
		"-config", configPath, "-entries", entriesPath, "-corpus", corpusPath,
		"-thresholds", "0.99,0", "-report-json", reportPath,
	}
	var stdout strings.Builder
	if err := cmdCalibrateTo(args, &stdout); err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	report, raw := readCalibrationReportForTest(t, reportPath)
	if err := validateCalibrationReport(report); err != nil {
		t.Fatalf("generated report invalid: %v\n%s", err, raw)
	}
	if !strings.Contains(stdout.String(), "review burden is within this returned cut") || !strings.Contains(stdout.String(), "truth rank p50/p99") {
		t.Fatalf("human report misses burden boundary:\n%s", stdout.String())
	}
	if report.Identity.ResolvedConfig.Match.DefaultThreshold != 0.6 || !reflect.DeepEqual(report.Identity.Thresholds, []float64{0, 0.99}) {
		t.Fatalf("default/applied thresholds conflated: %+v", report.Identity)
	}
	if report.Identity.ResolvedConfig.FilterableCount != 1 || report.Identity.ResolvedConfig.Match.TopK != 2 {
		t.Fatalf("resolved config identity = %+v", report.Identity.ResolvedConfig)
	}
	if report.Identity.Input.EntriesSHA256 != sha256Hex(entriesBytes) || report.Identity.Input.CorpusSHA256 != sha256Hex(corpusBytes) {
		t.Fatalf("file hashes = %+v", report.Identity.Input)
	}
	// The no-floor threshold must not silently mean the list default: id-2 is
	// a partial candidate for "alpha one" only in the loose row.
	if report.Results[0].Overall.Found <= report.Results[1].Overall.Found || report.Results[0].Overall.CandidateCountMax <= report.Results[1].Overall.CandidateCountMax {
		t.Fatalf("threshold 0 did not act as explicit no-floor: loose=%+v strict=%+v", report.Results[0].Overall, report.Results[1].Overall)
	}
	for _, secret := range []string{
		dir, configPath, entriesPath, corpusPath, reportPath,
		"private-config-name", "private-entries-name", "private-corpus-name", "private-report-name",
		"private-id-1", "private-id-2", "private-id-3", "private-query-needle", "payload-one",
		"private-scope-name", "private.scope.path",
	} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("calibration report leaked %q:\n%s", secret, raw)
		}
	}

	secondPath := filepath.Join(dir, "second.json")
	args[len(args)-1] = secondPath
	var secondStdout strings.Builder
	if err := cmdCalibrateTo(args, &secondStdout); err != nil {
		t.Fatalf("second calibrate: %v", err)
	}
	second, _ := readCalibrationReportForTest(t, secondPath)
	report.Observations = calibrationObservations{}
	second.Observations = calibrationObservations{}
	if !reflect.DeepEqual(report, second) {
		t.Fatalf("deterministic claims moved between identical runs:\nfirst:  %+v\nsecond: %+v", report, second)
	}
}

func TestCalibrationResolvedConfigUsesInstalledIdentity(t *testing.T) {
	match := engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, Threshold: 0.6, TopK: 10}
	preset, err := engine.NewList("preset", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"}, Match: match,
	})
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := engine.NewList("explicit", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: preset.Config().Analyzer.Steps}, Match: match,
	})
	if err != nil {
		t.Fatal(err)
	}
	makeReport := func(l *engine.List) calibrationReport {
		return makeCalibrationReport(
			l, strings.Repeat("a", 64), strings.Repeat("b", 64), 1, 1, 1,
			[]float64{0.4, 0.6}, nil, 0, 0, 0, nil,
		)
	}
	a := makeReport(preset)
	b := makeReport(explicit)
	if !reflect.DeepEqual(a.Identity.ResolvedConfig, b.Identity.ResolvedConfig) {
		t.Fatalf("preset spelling changed installed identity:\npreset:  %+v\nexplicit: %+v", a.Identity.ResolvedConfig, b.Identity.ResolvedConfig)
	}
}

func TestCalibrationInputIntegrity(t *testing.T) {
	validEntries := []engine.Entry{{ID: "a", Keys: []string{"alpha"}}}
	validCorpus := []bench.Case{{Query: "alpha", TruthID: "a", Category: "EXACT"}}
	if err := validateCalibrationInputs(validEntries, validCorpus); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		entries []engine.Entry
		corpus  []bench.Case
	}{
		{name: "empty entries", corpus: validCorpus},
		{name: "empty id", entries: []engine.Entry{{Keys: []string{"x"}}}, corpus: validCorpus},
		{name: "duplicate id", entries: []engine.Entry{{ID: "a"}, {ID: "a"}}, corpus: validCorpus},
		{name: "empty corpus", entries: validEntries},
		{name: "empty truth", entries: validEntries, corpus: []bench.Case{{Category: "X"}}},
		{name: "missing truth", entries: validEntries, corpus: []bench.Case{{TruthID: "b", Category: "X"}}},
		{name: "empty category", entries: validEntries, corpus: []bench.Case{{TruthID: "a"}}},
		{name: "reserved category", entries: validEntries, corpus: []bench.Case{{TruthID: "a", Category: "OVERALL"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateCalibrationInputs(tc.entries, tc.corpus); err == nil {
				t.Fatal("invalid inputs accepted")
			}
		})
	}
}

func TestCalibrationConfigDecoderIsStrict(t *testing.T) {
	dir := t.TempDir()
	for name, raw := range map[string]string{
		"unknown":  `{"analyzer":{"preset":"free-text"},"match":{"mode":"ngram"},"unknown":true}`,
		"trailing": `{"analyzer":{"preset":"free-text"},"match":{"mode":"ngram"}} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".json")
			mustWriteTestFile(t, path, []byte(raw))
			if _, err := readCalibrationConfig(path); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestCalibrateRejectsExactConfigWithoutOutput(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	entries := filepath.Join(dir, "entries.jsonl")
	corpus := filepath.Join(dir, "corpus.csv")
	target := filepath.Join(dir, "report.json")
	mustWriteTestFile(t, config, []byte(`{"analyzer":{"preset":"identifier"},"match":{"mode":"exact"}}`))
	mustWriteTestFile(t, entries, []byte("{\"id\":\"a\",\"keys\":[\"A-1\"]}\n"))
	mustWriteTestFile(t, corpus, []byte("query,truth_id,category\nA-1,a,EXACT\n"))
	var stdout strings.Builder
	err := cmdCalibrateTo([]string{
		"-config", config, "-entries", entries, "-corpus", corpus,
		"-thresholds", "0.4,0.6", "-report-json", target,
	}, &stdout)
	if err == nil || !strings.Contains(err.Error(), "requires ngram mode") {
		t.Fatalf("exact config error = %v", err)
	}
	assertNoFile(t, target)
	if stdout.Len() != 0 {
		t.Fatalf("failed calibration wrote stdout: %q", stdout.String())
	}
}

func TestCalibrationWalkthroughFixtures(t *testing.T) {
	fixtureDir := filepath.Join("..", "..", "docs", "calibration")
	target := filepath.Join(t.TempDir(), "calibration.json")
	var stdout strings.Builder
	if err := cmdCalibrateTo([]string{
		"-config", filepath.Join(fixtureDir, "list.json"),
		"-entries", filepath.Join(fixtureDir, "entries.jsonl"),
		"-corpus", filepath.Join(fixtureDir, "corpus.csv"),
		"-thresholds", "0.45,0.60,0.75",
		"-report-json", target,
	}, &stdout); err != nil {
		t.Fatalf("walkthrough command: %v", err)
	}
	report, _ := readCalibrationReportForTest(t, target)
	if err := validateCalibrationReport(report); err != nil {
		t.Fatalf("walkthrough report: %v", err)
	}
	if report.Identity.EntryCount != 5 || report.Identity.CorpusCaseCount != 7 || report.Identity.ResolvedConfig.FilterableCount != 1 {
		t.Fatalf("walkthrough identity = %+v", report.Identity)
	}
	if !reflect.DeepEqual(report.Identity.Thresholds, []float64{0.45, 0.6, 0.75}) {
		t.Fatalf("walkthrough thresholds = %v", report.Identity.Thresholds)
	}
	if !strings.Contains(stdout.String(), "ALIAS") || !strings.Contains(stdout.String(), "TYPO_1") {
		t.Fatalf("walkthrough categories absent from human output:\n%s", stdout.String())
	}
}

func TestCalibrateReportTargetFailureWritesNoHumanOutput(t *testing.T) {
	fixtureDir := filepath.Join("..", "..", "docs", "calibration")
	target := filepath.Join(t.TempDir(), "missing", "calibration.json")
	var stdout strings.Builder
	err := cmdCalibrateTo([]string{
		"-config", filepath.Join(fixtureDir, "list.json"),
		"-entries", filepath.Join(fixtureDir, "entries.jsonl"),
		"-corpus", filepath.Join(fixtureDir, "corpus.csv"),
		"-thresholds", "0.45,0.75",
		"-report-json", target,
	}, &stdout)
	if err == nil || !strings.Contains(err.Error(), "create temp") {
		t.Fatalf("unwritable target error = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("failed report wrote human stdout: %q", stdout.String())
	}
	assertNoFile(t, target)
}

func fixedCalibrationReport() calibrationReport {
	return calibrationReport{
		Schema: calibrationReportSchemaV1,
		Build: evalBuildIdentity{
			ModulePath: "example.test/kurn", Version: "(devel)", VCSRevision: "unknown", VCSModified: "unknown",
		},
		Environment: evalEnvironmentIdentity{
			GoVersion: "go1.26", GOOS: "linux", GOARCH: "amd64", LogicalCPU: 4, GOMAXPROCS: 2,
		},
		Identity: calibrationIdentity{
			BenchmarkMode: "ngram",
			ResolvedConfig: calibrationResolvedConfig{
				AnalyzerSteps: []string{"lowercase", "trim"},
				Match: calibrationMatchConfig{
					Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, DefaultThreshold: 0.6, TopK: 2,
				},
				FilterableCount: 1, DigestSHA256: strings.Repeat("a", 64),
			},
			Input: calibrationInputIdentity{
				Mode: "files", EntriesSHA256: strings.Repeat("b", 64), CorpusSHA256: strings.Repeat("c", 64),
			},
			Thresholds: []float64{0.4, 0.8}, EntryCount: 3, DistinctKeyCount: 3, CorpusCaseCount: 2,
		},
		Results: []calibrationThresholdResult{
			{
				Threshold: 0.4,
				Categories: []calibrationCategoryResult{
					{Category: "EXACT", Cases: 1, Found: 1, Recall: 1, CandidateCountP50: 1, CandidateCountP99: 1, CandidateCountMax: 1, TruthRankP50: 1, TruthRankP99: 1},
					{Category: "TYPO_1", Cases: 1, Found: 1, Recall: 1, CandidateCountP50: 2, CandidateCountP99: 2, CandidateCountMax: 2, TruthRankP50: 2, TruthRankP99: 2},
				},
				Overall: calibrationCategoryResult{Category: "OVERALL", Cases: 2, Found: 2, Recall: 1, CandidateCountP50: 2, CandidateCountP99: 2, CandidateCountMax: 2, TruthRankP50: 2, TruthRankP99: 2},
			},
			{
				Threshold: 0.8,
				Categories: []calibrationCategoryResult{
					{Category: "EXACT", Cases: 1, Found: 1, Recall: 1, CandidateCountP50: 1, CandidateCountP99: 1, CandidateCountMax: 1, TruthRankP50: 1, TruthRankP99: 1},
					{Category: "TYPO_1", Cases: 1, Misses: 1},
				},
				Overall: calibrationCategoryResult{Category: "OVERALL", Cases: 2, Found: 1, Misses: 1, Recall: 0.5, CandidateCountP50: 1, CandidateCountP99: 1, CandidateCountMax: 1, TruthRankP50: 1, TruthRankP99: 1},
			},
		},
		Observations: calibrationObservations{
			BuildDurationNS: 1000, IndexHeapDeltaBytes: 90, IndexBytesPerDistinctKey: 30, ProcessHeapAllocBytes: 200,
			Thresholds: []calibrationThresholdObservation{
				{Threshold: 0.4, LatencyP50US: 3, LatencyP99US: 7, QPS: 12.5},
				{Threshold: 0.8, LatencyP50US: 2, LatencyP99US: 5, QPS: 15},
			},
		},
	}
}

func TestCalibrationReportValidationAndStableBytes(t *testing.T) {
	report := fixedCalibrationReport()
	if err := validateCalibrationReport(report); err != nil {
		t.Fatalf("fixed report invalid: %v", err)
	}
	a, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("same typed report did not marshal deterministically")
	}
	want := `{
  "schema": "kurn-calibration/v1",
  "build": {
    "module_path": "example.test/kurn",
    "version": "(devel)",
    "vcs_revision": "unknown",
    "vcs_modified": "unknown"
  },
  "environment": {
    "go_version": "go1.26",
    "goos": "linux",
    "goarch": "amd64",
    "logical_cpu": 4,
    "gomaxprocs": 2
  },
  "identity": {
    "benchmark_mode": "ngram",
    "resolved_config": {
      "analyzer_steps": [
        "lowercase",
        "trim"
      ],
      "match": {
        "mode": "ngram",
        "fallback": "",
        "grams": [
          2,
          3
        ],
        "strip_spaces": true,
        "default_threshold": 0.6,
        "topk": 2
      },
      "filterable_count": 1,
      "golden_probe_count": 0,
      "overlay_auto_compact": 0,
      "digest_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    "input": {
      "mode": "files",
      "entries_sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      "corpus_sha256": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
    },
    "thresholds": [
      0.4,
      0.8
    ],
    "entry_count": 3,
    "distinct_key_count": 3,
    "corpus_case_count": 2
  },
  "results": [
    {
      "threshold": 0.4,
      "categories": [
        {
          "category": "EXACT",
          "cases": 1,
          "found": 1,
          "misses": 0,
          "recall": 1,
          "candidate_count_p50": 1,
          "candidate_count_p99": 1,
          "candidate_count_max": 1,
          "truth_rank_p50": 1,
          "truth_rank_p99": 1
        },
        {
          "category": "TYPO_1",
          "cases": 1,
          "found": 1,
          "misses": 0,
          "recall": 1,
          "candidate_count_p50": 2,
          "candidate_count_p99": 2,
          "candidate_count_max": 2,
          "truth_rank_p50": 2,
          "truth_rank_p99": 2
        }
      ],
      "overall": {
        "category": "OVERALL",
        "cases": 2,
        "found": 2,
        "misses": 0,
        "recall": 1,
        "candidate_count_p50": 2,
        "candidate_count_p99": 2,
        "candidate_count_max": 2,
        "truth_rank_p50": 2,
        "truth_rank_p99": 2
      }
    },
    {
      "threshold": 0.8,
      "categories": [
        {
          "category": "EXACT",
          "cases": 1,
          "found": 1,
          "misses": 0,
          "recall": 1,
          "candidate_count_p50": 1,
          "candidate_count_p99": 1,
          "candidate_count_max": 1,
          "truth_rank_p50": 1,
          "truth_rank_p99": 1
        },
        {
          "category": "TYPO_1",
          "cases": 1,
          "found": 0,
          "misses": 1,
          "recall": 0,
          "candidate_count_p50": 0,
          "candidate_count_p99": 0,
          "candidate_count_max": 0,
          "truth_rank_p50": 0,
          "truth_rank_p99": 0
        }
      ],
      "overall": {
        "category": "OVERALL",
        "cases": 2,
        "found": 1,
        "misses": 1,
        "recall": 0.5,
        "candidate_count_p50": 1,
        "candidate_count_p99": 1,
        "candidate_count_max": 1,
        "truth_rank_p50": 1,
        "truth_rank_p99": 1
      }
    }
  ],
  "observations": {
    "build_duration_ns": 1000,
    "index_heap_delta_bytes": 90,
    "index_bytes_per_distinct_key": 30,
    "process_heap_alloc_bytes": 200,
    "thresholds": [
      {
        "threshold": 0.4,
        "latency_p50_us": 3,
        "latency_p99_us": 7,
        "qps": 12.5
      },
      {
        "threshold": 0.8,
        "latency_p50_us": 2,
        "latency_p99_us": 5,
        "qps": 15
      }
    ]
  }
}`
	if string(a) != want {
		t.Fatalf("kurn-calibration/v1 bytes changed:\nwant:\n%s\ngot:\n%s", want, a)
	}

	for name, mutate := range map[string]func(*calibrationReport){
		"schema":          func(r *calibrationReport) { r.Schema = "kurn-calibration/v2" },
		"duplicate":       func(r *calibrationReport) { r.Identity.Thresholds[1] = r.Identity.Thresholds[0] },
		"threshold drift": func(r *calibrationReport) { r.Results[0].Threshold = 0.5 },
		"category order": func(r *calibrationReport) {
			r.Results[0].Categories[0], r.Results[0].Categories[1] = r.Results[0].Categories[1], r.Results[0].Categories[0]
		},
		"hidden miss":  func(r *calibrationReport) { r.Results[1].Overall.Misses = 0 },
		"rank on miss": func(r *calibrationReport) { r.Results[1].Categories[1].TruthRankP50 = 1 },
		"beyond topk":  func(r *calibrationReport) { r.Results[0].Overall.CandidateCountMax = 3 },
		"nan qps":      func(r *calibrationReport) { r.Observations.Thresholds[0].QPS = math.NaN() },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := fixedCalibrationReport()
			mutate(&invalid)
			if err := validateCalibrationReport(invalid); err == nil {
				t.Fatal("invalid report accepted")
			}
		})
	}
}

func TestCalibrationWriterFailsClosed(t *testing.T) {
	dir := t.TempDir()
	t.Run("create interruption", func(t *testing.T) {
		target := filepath.Join(dir, "create.json")
		mustWriteTestFile(t, target, []byte("sentinel"))
		ops := defaultCalibrationReportWriteOps
		ops.CreateTemp = func(string, string) (calibrationTempFile, error) {
			return nil, errors.New("injected create interruption")
		}
		if err := writeCalibrationReportWithOps(target, fixedCalibrationReport(), ops); err == nil || !strings.Contains(err.Error(), "injected create interruption") {
			t.Fatalf("create error = %v", err)
		}
		assertFileContents(t, target, "sentinel")
	})
	t.Run("short write", func(t *testing.T) {
		target := filepath.Join(dir, "short.json")
		mustWriteTestFile(t, target, []byte("sentinel"))
		ops := defaultCalibrationReportWriteOps
		ops.CreateTemp = func(dir, pattern string) (calibrationTempFile, error) {
			f, err := os.CreateTemp(dir, pattern)
			if err != nil {
				return nil, err
			}
			return shortEvalTempFile{File: f}, nil
		}
		if err := writeCalibrationReportWithOps(target, fixedCalibrationReport(), ops); err == nil || !strings.Contains(err.Error(), "short write") {
			t.Fatalf("short-write error = %v", err)
		}
		assertFileContents(t, target, "sentinel")
		assertNoEvalTemps(t, target)
	})
	for _, failAt := range []string{"sync", "close"} {
		t.Run(failAt+" interruption", func(t *testing.T) {
			target := filepath.Join(dir, failAt+".json")
			mustWriteTestFile(t, target, []byte("sentinel"))
			ops := defaultCalibrationReportWriteOps
			ops.CreateTemp = func(dir, pattern string) (calibrationTempFile, error) {
				f, err := os.CreateTemp(dir, pattern)
				if err != nil {
					return nil, err
				}
				return failingCalibrationTempFile{File: f, failAt: failAt}, nil
			}
			if err := writeCalibrationReportWithOps(target, fixedCalibrationReport(), ops); err == nil || !strings.Contains(err.Error(), "injected "+failAt+" interruption") {
				t.Fatalf("%s error = %v", failAt, err)
			}
			assertFileContents(t, target, "sentinel")
			assertNoEvalTemps(t, target)
		})
	}
	t.Run("rename interruption", func(t *testing.T) {
		target := filepath.Join(dir, "rename.json")
		mustWriteTestFile(t, target, []byte("sentinel"))
		ops := defaultCalibrationReportWriteOps
		ops.Rename = func(string, string) error { return errors.New("injected rename interruption") }
		if err := writeCalibrationReportWithOps(target, fixedCalibrationReport(), ops); err == nil || !strings.Contains(err.Error(), "injected rename interruption") {
			t.Fatalf("rename error = %v", err)
		}
		assertFileContents(t, target, "sentinel")
		assertNoEvalTemps(t, target)
	})
}

type failingCalibrationTempFile struct {
	*os.File
	failAt string
}

func (f failingCalibrationTempFile) Sync() error {
	if f.failAt == "sync" {
		return errors.New("injected sync interruption")
	}
	return f.File.Sync()
}

func (f failingCalibrationTempFile) Close() error {
	err := f.File.Close()
	if f.failAt == "close" {
		return errors.New("injected close interruption")
	}
	return err
}

func TestHumanCalibrationReportStableShape(t *testing.T) {
	report := fixedCalibrationReport()
	var out strings.Builder
	if err := writeHumanCalibrationReport(&out, report); err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"entries", "config digest", "review burden is within this returned cut",
		"threshold  category", "candidates p50/p99/max", "truth rank p50/p99", "0.4", "0.8", "QPS",
	} {
		if !strings.Contains(out.String(), needle) {
			t.Fatalf("human report missing %q:\n%s", needle, out.String())
		}
	}
	if strings.Contains(out.String(), "latency_p50_us") {
		t.Fatalf("human report unexpectedly emitted JSON field names:\n%s", out.String())
	}
}

func readCalibrationReportForTest(t *testing.T, path string) (calibrationReport, []byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report calibrationReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, raw)
	}
	return report, raw
}

func TestCalibrationPercentileRule(t *testing.T) {
	if got := percentileInts([]int{4, 1, 3, 2}, 50); got != 3 {
		t.Fatalf("upper median = %d, want 3", got)
	}
	if got := percentileInts([]int{4, 1, 3, 2}, 99); got != 4 {
		t.Fatalf("p99 = %d, want 4", got)
	}
	if got := percentileInts(nil, 50); got != 0 {
		t.Fatalf("empty percentile = %d, want 0", got)
	}
	if got := percentileInt64([]int64{7, 1, 3}, 50); got != 3 {
		t.Fatalf("int64 median = %d, want 3", got)
	}
}

func TestCalibrationBuildObservationIsFinite(t *testing.T) {
	report := fixedCalibrationReport()
	report.Observations.BuildDurationNS = time.Millisecond.Nanoseconds()
	if err := validateCalibrationReport(report); err != nil {
		t.Fatal(err)
	}
}
