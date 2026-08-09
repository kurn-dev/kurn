package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/kurn-dev/kurn/engine/analyzer"
)

func fixedEvalReport() evalReport {
	return evalReport{
		Schema: evalReportSchemaV1,
		Build: evalBuildIdentity{
			ModulePath:  "example.test/kurn",
			Version:     "(devel)",
			VCSRevision: "unknown",
			VCSModified: "unknown",
		},
		Environment: evalEnvironmentIdentity{
			GoVersion: "go1.26", GOOS: "linux", GOARCH: "amd64", LogicalCPU: 4, GOMAXPROCS: 2,
		},
		Identity: evalRunIdentity{
			BenchmarkMode: "ngram",
			ResolvedConfig: evalResolvedConfigIdentity{
				AnalyzerSteps: []string{"lowercase", "trim"},
				Match: evalMatchConfigIdentity{
					Mode: "ngram", Fallback: "", Grams: []int{2, 3}, StripSpaces: true, Threshold: 0.6, TopK: 100,
				},
				DigestSHA256: strings.Repeat("a", 64),
			},
			Input: evalInputIdentity{
				Mode: "synthetic",
				Synthetic: &evalSyntheticInputIdentity{
					Generator: evalSyntheticGeneratorV1, Seed: 42, Entries: 10, Sample: 1,
				},
			},
			EntryCount:       10,
			DistinctKeyCount: 9,
		},
		Results: evalResults{
			Categories: []evalCategoryResult{
				{Category: "EXACT", Cases: 1, Found: 1, Recall: 1},
				{Category: "TYPO_1", Cases: 1, Found: 0, Recall: 0},
			},
			Overall: evalCategoryResult{Category: "OVERALL", Cases: 2, Found: 1, Recall: 0.5},
		},
		Observations: evalObservations{
			BuildDurationNS:          1000,
			IndexHeapDeltaBytes:      90,
			IndexBytesPerDistinctKey: 10,
			ProcessHeapAllocBytes:    200,
			LatencyP50US:             3,
			LatencyP99US:             7,
			QPS:                      12.5,
		},
	}
}

func TestEvalReportV1ExactSchemaAndStableBytes(t *testing.T) {
	report := fixedEvalReport()
	if err := validateEvalReport(report); err != nil {
		t.Fatalf("fixed report does not satisfy its own schema: %v", err)
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	want := `{
  "schema": "kurn-eval/v1",
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
        "threshold": 0.6,
        "topk": 100
      },
      "digest_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    "input": {
      "mode": "synthetic",
      "synthetic": {
        "generator": "bench.Generate+bench.Corpus/v1",
        "seed": 42,
        "entries": 10,
        "sample": 1
      }
    },
    "entry_count": 10,
    "distinct_key_count": 9
  },
  "results": {
    "categories": [
      {
        "category": "EXACT",
        "cases": 1,
        "found": 1,
        "recall": 1
      },
      {
        "category": "TYPO_1",
        "cases": 1,
        "found": 0,
        "recall": 0
      }
    ],
    "overall": {
      "category": "OVERALL",
      "cases": 2,
      "found": 1,
      "recall": 0.5
    }
  },
  "observations": {
    "build_duration_ns": 1000,
    "index_heap_delta_bytes": 90,
    "index_bytes_per_distinct_key": 10,
    "process_heap_alloc_bytes": 200,
    "latency_p50_us": 3,
    "latency_p99_us": 7,
    "qps": 12.5
  }
}
`
	if string(raw) != want {
		t.Fatalf("kurn-eval/v1 bytes changed (-want +got):\nwant:\n%s\ngot:\n%s", want, raw)
	}
	raw2, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw2)+"\n" != want {
		t.Fatal("same typed report did not marshal deterministically")
	}
}

func TestEvalResolvedConfigUsesInstalledIdentity(t *testing.T) {
	steps, ok := analyzer.PresetSteps("person-name")
	if !ok {
		t.Fatal("person-name preset missing")
	}
	match := engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, Threshold: 0.5, TopK: 100}
	preset, err := engine.NewList("preset", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"}, Match: match,
	})
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := engine.NewList("explicit", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: steps}, Match: match,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := evalInputIdentity{Mode: "synthetic", Synthetic: &evalSyntheticInputIdentity{
		Generator: evalSyntheticGeneratorV1, Seed: 42, Entries: 1, Sample: 1,
	}}
	rep := bench.Report{
		Cases: 1, Found: 1, Recall: 1,
		ByCategory: map[string]*bench.CatStats{"EXACT": {Cases: 1, Found: 1, Recall: 1}},
	}
	a := makeEvalReport(preset, input, 1, 1, 0, 0, 0, rep)
	b := makeEvalReport(explicit, input, 1, 1, 0, 0, 0, rep)
	if !reflect.DeepEqual(a.Identity.ResolvedConfig, b.Identity.ResolvedConfig) {
		t.Fatalf("preset spelling changed installed identity:\npreset:  %+v\nexplicit: %+v", a.Identity.ResolvedConfig, b.Identity.ResolvedConfig)
	}
}

func TestEvalCategoryOrderIsCanonicalThenLexical(t *testing.T) {
	rep := bench.Report{ByCategory: map[string]*bench.CatStats{
		"ZETA":   {Cases: 1},
		"TYPO_1": {Cases: 1},
		"ALPHA":  {Cases: 1},
		"EXACT":  {Cases: 1},
	}}
	got := orderedEvalCategories(rep)
	var names []string
	for _, result := range got {
		names = append(names, result.Category)
	}
	want := []string{"EXACT", "TYPO_1", "ALPHA", "ZETA"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("category order = %v, want %v", names, want)
	}
}

func TestHumanBenchReportLegacyBytes(t *testing.T) {
	rep := bench.Report{
		Cases: 2, Found: 1, Recall: 0.5,
		ByCategory: map[string]*bench.CatStats{
			"EXACT":  {Cases: 1, Found: 1, Recall: 1},
			"TYPO_1": {Cases: 1, Found: 0, Recall: 0},
		},
		P50Us: 3, P99Us: 7, QPS: 12.5,
	}
	var out strings.Builder
	if err := writeHumanBenchReport(&out, 2, 2, 1500*time.Microsecond, 45, 2<<20, rep); err != nil {
		t.Fatal(err)
	}
	want := "entries            2\n" +
		"distinct keys      2\n" +
		"build time         2ms\n" +
		"index B/key        45.0  (approx: HeapAlloc delta around Replace, entries excluded)\n" +
		"process HeapAlloc  2.0 MiB\n" +
		"\n" +
		"category  cases  found  recall\n" +
		"EXACT     1      1      1.000\n" +
		"TYPO_1    1      0      0.000\n" +
		"OVERALL   2      1      0.500\n" +
		"\n" +
		"p50  3 µs\n" +
		"p99  7 µs\n" +
		"QPS  12\n"
	if out.String() != want {
		t.Fatalf("legacy human report bytes changed:\nwant:\n%q\ngot:\n%q", want, out.String())
	}
}

func TestBenchReportSyntheticMode(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "synthetic.json")
	var stdout strings.Builder
	args := []string{
		"-n", "64", "-sample", "1", "-seed", "42", "-mode", "exact", "-report-json", reportPath,
	}
	err := cmdBenchTo(args, &stdout)
	if err != nil {
		t.Fatalf("bench synthetic report: %v", err)
	}
	report, raw := readEvalReportForTest(t, reportPath)
	if err := validateEvalReport(report); err != nil {
		t.Fatalf("generated report invalid: %v\n%s", err, raw)
	}
	if report.Identity.Input.Mode != "synthetic" || report.Identity.Input.Synthetic == nil || report.Identity.Input.Files != nil {
		t.Fatalf("synthetic identity populated wrong fields: %+v", report.Identity.Input)
	}
	if got := report.Identity.Input.Synthetic; got.Seed != 42 || got.Entries != 64 || got.Sample != 1 {
		t.Fatalf("synthetic generator identity = %+v", got)
	}
	if got := report.Identity.ResolvedConfig.Match.Grams; !reflect.DeepEqual(got, []int{2, 3}) {
		t.Fatalf("exact-mode resolved grams = %#v, want the installed [2 3] config", got)
	}
	if !strings.Contains(stdout.String(), "entries") || !strings.Contains(stdout.String(), "OVERALL") || !strings.Contains(stdout.String(), "QPS") {
		t.Fatalf("human stdout missing legacy sections:\n%s", stdout.String())
	}
	assertFiniteObservations(t, report.Observations)

	// The same pinned generator/config has byte-stable claims even though
	// allocator and timing observations are deliberately not identity.
	secondPath := filepath.Join(dir, "synthetic-second.json")
	args[len(args)-1] = secondPath
	var secondStdout strings.Builder
	if err := cmdBenchTo(args, &secondStdout); err != nil {
		t.Fatalf("second pinned synthetic report: %v", err)
	}
	second, _ := readEvalReportForTest(t, secondPath)
	report.Observations = evalObservations{}
	second.Observations = evalObservations{}
	if !reflect.DeepEqual(report, second) {
		t.Fatalf("deterministic report claims moved between identical runs:\nfirst:  %+v\nsecond: %+v", report, second)
	}
}

func TestBenchReportFileHashesAndPrivacy(t *testing.T) {
	dir := t.TempDir()
	entriesBytes := []byte("{\"id\":\"raw-entry-secret\",\"keys\":[\"Raw Secret Name\"]}\n")
	corpusBytes := []byte("query,truth_id,category\nRaw Secret Name,raw-entry-secret,EXACT\n")
	entriesPath := filepath.Join(dir, "source-entries-secret.jsonl")
	corpusPath := filepath.Join(dir, "source-corpus-secret.csv")
	reportPath := filepath.Join(dir, "target-report-secret.json")
	mustWriteTestFile(t, entriesPath, entriesBytes)
	mustWriteTestFile(t, corpusPath, corpusBytes)

	var stdout strings.Builder
	if err := cmdBenchTo([]string{
		"-entries", entriesPath, "-corpus", corpusPath, "-mode", "exact", "-report-json", reportPath,
	}, &stdout); err != nil {
		t.Fatalf("bench file report: %v", err)
	}
	report, raw := readEvalReportForTest(t, reportPath)
	if report.Identity.Input.Mode != "files" || report.Identity.Input.Files == nil || report.Identity.Input.Synthetic != nil {
		t.Fatalf("file identity populated wrong fields: %+v", report.Identity.Input)
	}
	wantEntries := sha256Hex(entriesBytes)
	wantCorpus := sha256Hex(corpusBytes)
	if report.Identity.Input.Files.EntriesSHA256 != wantEntries || report.Identity.Input.Files.CorpusSHA256 != wantCorpus {
		t.Fatalf("input hashes = %+v, want entries=%s corpus=%s", report.Identity.Input.Files, wantEntries, wantCorpus)
	}
	for _, secret := range []string{
		entriesPath, corpusPath, reportPath, dir,
		"source-entries-secret", "source-corpus-secret", "target-report-secret",
		"raw-entry-secret", "Raw Secret Name",
	} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("report leaked %q:\n%s", secret, raw)
		}
	}

	entriesCopy := filepath.Join(dir, "copy.jsonl")
	corpusCopy := filepath.Join(dir, "copy.csv")
	mustWriteTestFile(t, entriesCopy, entriesBytes)
	mustWriteTestFile(t, corpusCopy, corpusBytes)
	_, entriesHash, err := readEntriesWithSHA256(entriesCopy)
	if err != nil {
		t.Fatal(err)
	}
	_, corpusHash, err := readCorpusWithSHA256(corpusCopy)
	if err != nil {
		t.Fatal(err)
	}
	if entriesHash != wantEntries || corpusHash != wantCorpus {
		t.Fatal("same input bytes at different paths changed identity")
	}
	mutated := bytes.Replace(entriesBytes, []byte("Name"), []byte("Namf"), 1)
	mutatedPath := filepath.Join(dir, "mutated.jsonl")
	mustWriteTestFile(t, mutatedPath, mutated)
	_, mutatedHash, err := readEntriesWithSHA256(mutatedPath)
	if err != nil {
		t.Fatal(err)
	}
	if mutatedHash == wantEntries {
		t.Fatal("entry-byte mutation did not change identity")
	}
	mutatedCorpus := bytes.Replace(corpusBytes, []byte("EXACT"), []byte("OTHER"), 1)
	mutatedCorpusPath := filepath.Join(dir, "mutated.csv")
	mustWriteTestFile(t, mutatedCorpusPath, mutatedCorpus)
	_, mutatedCorpusHash, err := readCorpusWithSHA256(mutatedCorpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if mutatedCorpusHash == wantCorpus {
		t.Fatal("corpus-byte mutation did not change identity")
	}
}

func TestBenchReportInvalidInputsFailWithoutFinalArtifact(t *testing.T) {
	dir := t.TempDir()
	validEntries := filepath.Join(dir, "entries.jsonl")
	mustWriteTestFile(t, validEntries, []byte("{\"id\":\"id\",\"keys\":[\"x\"]}\n"))
	validCorpus := filepath.Join(dir, "corpus.csv")
	mustWriteTestFile(t, validCorpus, []byte("query,truth_id,category\nx,id,EXACT\n"))
	for _, tc := range []struct {
		name    string
		entries []byte
	}{
		{name: "empty", entries: nil},
		{name: "malformed", entries: []byte("{not-json\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := filepath.Join(dir, tc.name+".jsonl")
			target := filepath.Join(dir, tc.name+".json")
			mustWriteTestFile(t, entries, tc.entries)
			var stdout strings.Builder
			err := cmdBenchTo([]string{
				"-entries", entries, "-corpus", validCorpus, "-mode", "exact", "-report-json", target,
			}, &stdout)
			if err == nil {
				t.Fatal("invalid input unexpectedly succeeded")
			}
			assertNoFile(t, target)
			if stdout.Len() != 0 {
				t.Fatalf("failed report wrote human stdout: %q", stdout.String())
			}
		})
	}
	for _, tc := range []struct {
		name   string
		corpus []byte
	}{
		{name: "empty-corpus", corpus: nil},
		{name: "malformed-corpus", corpus: []byte("wrong,header\nx,id\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			corpus := filepath.Join(dir, tc.name+".csv")
			target := filepath.Join(dir, tc.name+".json")
			mustWriteTestFile(t, corpus, tc.corpus)
			var stdout strings.Builder
			err := cmdBenchTo([]string{
				"-entries", validEntries, "-corpus", corpus, "-mode", "exact", "-report-json", target,
			}, &stdout)
			if err == nil {
				t.Fatal("invalid corpus unexpectedly succeeded")
			}
			assertNoFile(t, target)
			if stdout.Len() != 0 {
				t.Fatalf("failed report wrote human stdout: %q", stdout.String())
			}
		})
	}

	for _, args := range [][]string{
		{"-n", "0", "-report-json", filepath.Join(dir, "zero.json")},
		{"-n", "64", "-sample", "1", "-report-json", "-"},
	} {
		var stdout strings.Builder
		if err := cmdBenchTo(args, &stdout); err == nil {
			t.Fatalf("invalid args %v unexpectedly succeeded", args)
		}
		if stdout.Len() != 0 {
			t.Fatalf("invalid args %v wrote stdout: %q", args, stdout.String())
		}
	}
	assertNoFile(t, filepath.Join(dir, "zero.json"))
}

func TestBenchReportUnwritableTargetFailsWithoutFinalArtifact(t *testing.T) {
	target := filepath.Join(t.TempDir(), "missing-parent", "report.json")
	var stdout strings.Builder
	err := cmdBenchTo([]string{
		"-n", "64", "-sample", "1", "-mode", "exact", "-report-json", target,
	}, &stdout)
	if err == nil || !strings.Contains(err.Error(), "create temp") {
		t.Fatalf("unwritable target error = %v", err)
	}
	assertNoFile(t, target)
	if stdout.Len() != 0 {
		t.Fatalf("failed report wrote human stdout: %q", stdout.String())
	}
}

func TestEvalReportWriterFailsClosed(t *testing.T) {
	dir := t.TempDir()
	t.Run("unsupported schema", func(t *testing.T) {
		target := filepath.Join(dir, "unsupported.json")
		report := fixedEvalReport()
		report.Schema = "kurn-eval/v2"
		if err := writeEvalReport(target, report); err == nil || !strings.Contains(err.Error(), "unsupported schema") {
			t.Fatalf("unsupported schema error = %v", err)
		}
		assertNoFile(t, target)
	})

	t.Run("invalid observations", func(t *testing.T) {
		for name, mutate := range map[string]func(*evalReport){
			"nan":      func(r *evalReport) { r.Observations.QPS = math.NaN() },
			"infinite": func(r *evalReport) { r.Observations.IndexBytesPerDistinctKey = math.Inf(1) },
			"negative": func(r *evalReport) { r.Observations.BuildDurationNS = -1 },
		} {
			t.Run(name, func(t *testing.T) {
				target := filepath.Join(dir, "invalid-"+name+".json")
				report := fixedEvalReport()
				mutate(&report)
				if err := writeEvalReport(target, report); err == nil {
					t.Fatal("invalid observations unexpectedly written")
				}
				assertNoFile(t, target)
			})
		}
	})

	t.Run("short write", func(t *testing.T) {
		target := filepath.Join(dir, "short.json")
		mustWriteTestFile(t, target, []byte("sentinel"))
		ops := defaultEvalReportWriteOps
		ops.CreateTemp = func(dir, pattern string) (evalTempFile, error) {
			f, err := os.CreateTemp(dir, pattern)
			if err != nil {
				return nil, err
			}
			return shortEvalTempFile{File: f}, nil
		}
		if err := writeEvalReportWithOps(target, fixedEvalReport(), ops); err == nil || !strings.Contains(err.Error(), "short write") {
			t.Fatalf("short-write error = %v", err)
		}
		assertFileContents(t, target, "sentinel")
		assertNoEvalTemps(t, target)
	})

	t.Run("rename interruption", func(t *testing.T) {
		target := filepath.Join(dir, "rename.json")
		mustWriteTestFile(t, target, []byte("sentinel"))
		ops := defaultEvalReportWriteOps
		ops.Rename = func(string, string) error { return errors.New("injected rename interruption") }
		if err := writeEvalReportWithOps(target, fixedEvalReport(), ops); err == nil || !strings.Contains(err.Error(), "injected rename interruption") {
			t.Fatalf("rename error = %v", err)
		}
		assertFileContents(t, target, "sentinel")
		assertNoEvalTemps(t, target)
	})
}

type shortEvalTempFile struct{ *os.File }

func (f shortEvalTempFile) Write(p []byte) (int, error) {
	return f.File.Write(p[:len(p)/2])
}

func readEvalReportForTest(t *testing.T, path string) (evalReport, []byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report evalReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, raw)
	}
	return report, raw
}

func assertFiniteObservations(t *testing.T, o evalObservations) {
	t.Helper()
	if o.BuildDurationNS < 0 || o.IndexHeapDeltaBytes < 0 || o.LatencyP50US < 0 || o.LatencyP99US < 0 ||
		!finiteNonnegative(o.IndexBytesPerDistinctKey) || !finiteNonnegative(o.QPS) {
		t.Fatalf("observations are not finite and non-negative: %+v", o)
	}
}

func sha256Hex(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func mustWriteTestFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("final artifact exists after failure: %s (%v)", path, err)
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != want {
		t.Fatalf("%s = %q, want %q", path, raw, want)
	}
}

func assertNoEvalTemps(t *testing.T, target string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary report artifacts remain: %v", matches)
	}
}
