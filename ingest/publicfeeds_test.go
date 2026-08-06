package ingest_test

// The public-feed mappings (LEIE, SAM exclusions) and the two
// ingest features they forced: composite entry IDs (id_paths — LEIE has
// no stable single-column ID) and byte-identical duplicate collapse (the
// real LEIE file ships literal duplicate rows). Fixture rows are
// FICTIONAL in real column layouts; the real feeds stay out of the repo
// and are exercised by env-gated tests on locally downloaded copies.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/ingest"
)

func loadMapping(t *testing.T, name string) *ingest.Mapping {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "docs", "examples", name))
	if err != nil {
		t.Fatal(err)
	}
	var m ingest.Mapping
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("%s invalid: %v", name, err)
	}
	return &m
}

const leieHeader = `LASTNAME,FIRSTNAME,MIDNAME,BUSNAME,GENERAL,SPECIALTY,UPIN,NPI,DOB,ADDRESS,CITY,STATE,ZIP,EXCLTYPE,EXCLDATE,REINDATE,WAIVERDATE,WVRSTATE`

// One person, one business, then the person row again byte-identically
// (the real feed's literal-duplicate shape).
const leieFixture = leieHeader + `
VASQUEZ,ELENA,,,EMPLOYEE - MD,INTERNAL MEDICINE,,0000000000,19700101,1 CARE WAY,SPRINGFIELD,ZZ,00001,1128a1,20250102,00000000,00000000,
,,,IRIS BELL HOME HEALTH LLC,OTHER BUSINESS,HOME HEALTH AGENCY,,0000000000,,2 WELLNESS RD,SPRINGFIELD,ZZ,00002,1128b8,20240315,00000000,00000000,
VASQUEZ,ELENA,,,EMPLOYEE - MD,INTERNAL MEDICINE,,0000000000,19700101,1 CARE WAY,SPRINGFIELD,ZZ,00001,1128a1,20250102,00000000,00000000,
`

func TestLEIEMappingCompositeIDAndKeys(t *testing.T) {
	m := loadMapping(t, "leie.mapping.json")
	var entries []engine.Entry
	if _, err := ingest.Parse(m, strings.NewReader(leieFixture), ingest.Options{}, func(e engine.Entry) error {
		entries = append(entries, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Parse yields all three (collapse is the BUILDER's job).
	if len(entries) != 3 {
		t.Fatalf("entries: %d, want 3", len(entries))
	}
	person, business := entries[0], entries[1]
	wantID := "VASQUEZ|ELENA|||EMPLOYEE - MD|INTERNAL MEDICINE||0000000000|19700101|1 CARE WAY|SPRINGFIELD|ZZ|00001|1128a1|20250102"
	if person.ID != wantID {
		t.Fatalf("composite id:\n got %q\nwant %q", person.ID, wantID)
	}
	// Person: name key only (BUSNAME empty rule yields nothing);
	// business: the inverse.
	if len(person.Keys) != 1 || person.Keys[0] != "ELENA VASQUEZ" {
		t.Fatalf("person keys: %v", person.Keys)
	}
	if len(business.Keys) != 1 || business.Keys[0] != "IRIS BELL HOME HEALTH LLC" {
		t.Fatalf("business keys: %v", business.Keys)
	}
	var payload map[string]string
	if err := json.Unmarshal(person.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["excl_type"] != "1128a1" || payload["state"] != "ZZ" {
		t.Fatalf("payload: %v", payload)
	}
}

func TestBuildCollapsesIdenticalDuplicates(t *testing.T) {
	m := loadMapping(t, "leie.mapping.json")
	out := filepath.Join(t.TempDir(), "bundle")
	man, err := ingest.Build(m, strings.NewReader(leieFixture), out, ingest.BuildOptions{Source: "leie-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if man.Entries != 2 || man.Collapsed != 1 {
		t.Fatalf("manifest: entries %d collapsed %d, want 2/1", man.Entries, man.Collapsed)
	}

	// CONFLICTING duplicates (same ID, different content) still fail the
	// build loudly — collapse never becomes last-wins.
	conflict := &ingest.Mapping{
		Format: "ndjson", ID: "id", Keys: []ingest.KeyRule{{Path: "k"}},
		List: m.List,
	}
	feed := `{"id":"x","k":"Toni Brook"}
{"id":"x","k":"Piotr Wexler"}`
	if _, err := ingest.Build(conflict, strings.NewReader(feed), filepath.Join(t.TempDir(), "b"), ingest.BuildOptions{}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("conflicting duplicate accepted: %v", err)
	}
}

func TestCompositeIDValidation(t *testing.T) {
	base := func() *ingest.Mapping {
		return &ingest.Mapping{
			Format: "ndjson", Keys: []ingest.KeyRule{{Path: "k"}},
			List: engine.ListConfig{
				Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
				Match:    engine.MatchConfig{Mode: "ngram"},
			},
		}
	}
	both := base()
	both.ID = "id"
	both.IDPaths = []string{"a", "b"}
	if err := both.Validate(); err == nil {
		t.Fatal("id AND id_paths accepted")
	}
	neither := base()
	if err := neither.Validate(); err == nil {
		t.Fatal("neither id nor id_paths accepted")
	}
	emptyPath := base()
	emptyPath.IDPaths = []string{"a", ""}
	if err := emptyPath.Validate(); err == nil {
		t.Fatal("empty id_paths component accepted")
	}

	// An all-empty composite is a bad record, not a silent "" ID.
	ok := base()
	ok.IDPaths = []string{"missing1", "missing2"}
	feed := `{"k":"Iris Bell"}`
	if _, err := ingest.Parse(ok, strings.NewReader(feed), ingest.Options{}, func(engine.Entry) error { return nil }); err == nil || !strings.Contains(err.Error(), "composite id empty") {
		t.Fatalf("all-empty composite id accepted: %v", err)
	}
}

// SAM fixture: fictional rows in the documented Public Extract V2 column
// layout (headers must be re-verified against a real extract — the
// download is behind a free SAM account; see docs/examples/README.md).
const samFixture = `Classification,Name,Prefix,First,Middle,Last,Suffix,Address 1,City,State / Province,Country,Zip Code,DUNS,UEI,Exclusion Program,Excluding Agency,CT Code,Exclusion Type,Additional Comments,Active Date,Termination Date,Record Status,Cross-Reference,SAM Number,CAGE,NPI
Individual,,Dr,PIOTR,,WEXLER,,9 HARBOR LN,SPRINGFIELD,ZZ,USA,00003,,WEX000000001,Reciprocal,TREAS-OFAC,,Ineligible (Proceedings Completed),,2024-06-01,Indefinite,Active,,S4MR0000001,,0000000000
Firm,TONI BROOK LOGISTICS LLC,,,,,,4 DOCK ST,SPRINGFIELD,ZZ,USA,00004,,BRK000000002,Procurement,DOD,,Ineligible (Proceedings Completed),,2023-11-12,2027-11-12,Active,,S4MR0000002,,0000000000
`

func TestSAMMappingFixture(t *testing.T) {
	m := loadMapping(t, "sam-exclusions.mapping.json")
	var entries []engine.Entry
	if _, err := ingest.Parse(m, strings.NewReader(samFixture), ingest.Options{}, func(e engine.Entry) error {
		entries = append(entries, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries: %d, want 2", len(entries))
	}
	if entries[0].ID != "S4MR0000001" || entries[0].Keys[0] != "PIOTR WEXLER" {
		t.Fatalf("individual: %+v", entries[0])
	}
	if entries[1].ID != "S4MR0000002" || entries[1].Keys[0] != "TONI BROOK LOGISTICS LLC" {
		t.Fatalf("firm: %+v", entries[1])
	}
}

// The real feeds, env-gated (public data stays out of the repo).

func TestLEIERealFeed(t *testing.T) {
	path := os.Getenv("KURN_LEIE_CSV")
	if path == "" {
		t.Skip("set KURN_LEIE_CSV to a downloaded LEIE UPDATED.csv to run")
	}
	m := loadMapping(t, "leie.mapping.json")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := filepath.Join(t.TempDir(), "bundle")
	man, err := ingest.Build(m, f, out, ingest.BuildOptions{Source: "LEIE " + filepath.Base(path)})
	if err != nil {
		t.Fatal(err)
	}
	if man.Entries < 50000 {
		t.Fatalf("suspiciously small LEIE build: %d entries", man.Entries)
	}
	t.Logf("LEIE bundle: %d entries, %d keys, %d duplicate rows collapsed, version %s",
		man.Entries, man.Keys, man.Collapsed, man.VersionID)

	// Provenance + a live query through the built bundle: load it the way
	// the platform's replay worker does (bundle dir == list dir).
	dataDir := t.TempDir()
	if err := os.Rename(out, filepath.Join(dataDir, "leie")); err != nil {
		t.Fatal(err)
	}
	st, err := engine.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := st.List("leie")
	if !ok {
		t.Fatal("bundle did not load as a list")
	}
	if !strings.HasPrefix(l.Version(), man.SHA256+"@") {
		t.Fatalf("provenance: stamp %q vs manifest sha256 %s", l.Version(), man.SHA256)
	}
	if !strings.HasPrefix(l.Version(), man.VersionID) {
		t.Fatalf("display prefix: stamp %q vs manifest version_id %s", l.Version(), man.VersionID)
	}
	if !strings.HasSuffix(l.Version(), "+c"+man.ConfigSHA256) {
		t.Fatalf("provenance: stamp %q does not carry manifest config_sha256 %s", l.Version(), man.ConfigSHA256)
	}
}

func TestSAMRealFeed(t *testing.T) {
	path := os.Getenv("KURN_SAM_CSV")
	if path == "" {
		t.Skip("set KURN_SAM_CSV to a downloaded SAM exclusions Public Extract V2 CSV to run")
	}
	m := loadMapping(t, "sam-exclusions.mapping.json")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := filepath.Join(t.TempDir(), "bundle")
	man, err := ingest.Build(m, f, out, ingest.BuildOptions{Source: "SAM " + filepath.Base(path)})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SAM bundle: %d entries, %d keys, version %s", man.Entries, man.Keys, man.VersionID)
}

// KeyRule.Split: CSV feeds carry aliases as one delimited column
// ("A; B; C"). Each piece must become its own key, or the column is a
// single unmatchable blob.
func TestKeyRuleSplit(t *testing.T) {
	m := &ingest.Mapping{
		Format: "csv", ID: "id",
		Keys: []ingest.KeyRule{
			{Path: "name"},
			{Path: "alt_names", Split: ";"},
		},
		List: engine.ListConfig{
			Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
			Match:    engine.MatchConfig{Mode: "ngram"},
		},
	}
	feed := "id,name,alt_names\n" +
		"1,Bank of Meridian,\"Veldano Urban Credit; Veldano City Commercial Bank ;\"\n" +
		"2,Iris Bell,\n"
	var got []engine.Entry
	if _, err := ingest.Parse(m, strings.NewReader(feed), ingest.Options{}, func(e engine.Entry) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"Bank of Meridian", "Veldano Urban Credit", "Veldano City Commercial Bank"}
	if len(got) != 2 || len(got[0].Keys) != 3 {
		t.Fatalf("keys: %v", got[0].Keys)
	}
	for i, w := range want {
		if got[0].Keys[i] != w {
			t.Fatalf("key %d = %q, want %q (trailing empty piece must be dropped)", i, got[0].Keys[i], w)
		}
	}
	if len(got[1].Keys) != 1 {
		t.Fatalf("empty alias column should yield no extra keys: %v", got[1].Keys)
	}

	// split with multiple paths is ambiguous and must be refused
	bad := *m
	bad.Keys = []ingest.KeyRule{{Paths: []string{"a", "b"}, Split: ";"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("split over joined paths accepted")
	}
}
