package ingest_test

// Mapping validation, per-instance filter semantics (the aka
// shape), joins, record-level where, all three formats, the record bound,
// and fail-by-default vs -skip-bad.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/ingest"
)

func exactList() engine.ListConfig {
	return engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "exact"},
	}
}

func collect(t *testing.T, m *ingest.Mapping, input string, opts ingest.Options) ([]engine.Entry, ingest.Stats, error) {
	t.Helper()
	var out []engine.Entry
	st, err := ingest.Parse(m, strings.NewReader(input), opts, func(e engine.Entry) error {
		out = append(out, e)
		return nil
	})
	return out, st, err
}

// The OFAC shape in miniature: record filter, joined primary name, and
// per-instance alias filtering — the aka with category "weak" and the
// f.k.a. must behave per-element, not leak across siblings.
const akaXML = `<?xml version="1.0"?>
<list xmlns="http://example.com/ns">
  <entry><uid>1</uid><type>Individual</type>
    <firstName>Anna</firstName><lastName>Smith</lastName>
    <akaList>
      <aka><cat>strong</cat><kind>a.k.a.</kind><first>Ann</first><last>Smythe</last></aka>
      <aka><cat>weak</cat><kind>a.k.a.</kind><first>A.</first><last>S.</last></aka>
      <aka><cat>strong</cat><kind>f.k.a.</kind><first>Anya</first><last>Schmidt</last></aka>
    </akaList>
  </entry>
  <entry><uid>2</uid><type>Entity</type>
    <lastName>ACME CORP</lastName>
  </entry>
  <entry><uid>3</uid><type>Individual</type>
    <firstName>Bo</firstName><lastName>Chan</lastName>
  </entry>
</list>`

func akaMapping() *ingest.Mapping {
	return &ingest.Mapping{
		Format: "xml",
		Record: "entry",
		Where:  map[string]string{"type": "Individual"},
		ID:     "uid",
		Keys: []ingest.KeyRule{
			{Paths: []string{"firstName", "lastName"}},
			{Paths: []string{"akaList.aka.first", "akaList.aka.last"},
				Where: map[string]string{"akaList.aka.cat": "strong", "akaList.aka.kind": "a.k.a."}},
			{Paths: []string{"akaList.aka.first", "akaList.aka.last"},
				Where: map[string]string{"akaList.aka.cat": "strong", "akaList.aka.kind": "f.k.a."}},
		},
		Payload: map[string]string{"kind": "type"},
		List:    exactList(),
	}
}

func TestXMLPerInstanceFilters(t *testing.T) {
	entries, st, err := collect(t, akaMapping(), akaXML, ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Records != 2 || st.Filtered != 1 || st.Bad != 0 {
		t.Fatalf("stats = %+v, want 2 records, 1 filtered", st)
	}
	if entries[0].ID != "1" {
		t.Fatalf("entry 0 id = %s", entries[0].ID)
	}
	// Weak aka excluded, strong a.k.a. and f.k.a. joined per instance —
	// never "Ann Schmidt" (components crossing aka siblings).
	want := []string{"Anna Smith", "Ann Smythe", "Anya Schmidt"}
	if fmt.Sprint(entries[0].Keys) != fmt.Sprint(want) {
		t.Fatalf("keys = %v, want %v", entries[0].Keys, want)
	}
	if string(entries[0].Payload) != `{"kind":"Individual"}` {
		t.Fatalf("payload = %s", entries[0].Payload)
	}
	// Entry 3 has no akas: primary key only.
	if len(entries[1].Keys) != 1 || entries[1].Keys[0] != "Bo Chan" {
		t.Fatalf("entry 3 keys = %v", entries[1].Keys)
	}
}

func TestNDJSONArraysAndNumbers(t *testing.T) {
	input := `{"id": 101, "name": "acme", "alt": [{"v":"acme inc","ok":true},{"v":"acmeco","ok":false}]}
{"id": 102, "name": "globex", "alt": []}`
	m := &ingest.Mapping{
		Format: "ndjson",
		ID:     "id",
		Keys: []ingest.KeyRule{
			{Path: "name"},
			{Path: "alt.v", Where: map[string]string{"alt.ok": "true"}},
		},
		List: exactList(),
	}
	entries, st, err := collect(t, m, input, ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Records != 2 {
		t.Fatalf("stats %+v", st)
	}
	// json.Number keeps "101" verbatim; per-instance filter keeps only ok:true.
	if entries[0].ID != "101" || fmt.Sprint(entries[0].Keys) != `[acme acme inc]` {
		t.Fatalf("entry 0 = %+v", entries[0])
	}
	if fmt.Sprint(entries[1].Keys) != `[globex]` {
		t.Fatalf("entry 1 keys = %v", entries[1].Keys)
	}
}

func TestCSVColumnsVerbatim(t *testing.T) {
	input := "id,full.name,country\nc1,Jane Doe,EE\nc2,John Roe,US\n"
	m := &ingest.Mapping{
		Format: "csv",
		ID:     "id",
		Keys:   []ingest.KeyRule{{Path: "full.name"}}, // dot in a column name: verbatim, not a path
		Where:  map[string]string{"country": "EE"},
		List:   exactList(),
	}
	entries, st, err := collect(t, m, input, ingest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Records != 1 || st.Filtered != 1 {
		t.Fatalf("stats %+v", st)
	}
	if entries[0].ID != "c1" || entries[0].Keys[0] != "Jane Doe" {
		t.Fatalf("entry = %+v", entries[0])
	}
}

func TestBadRecordsFailByDefault(t *testing.T) {
	input := "{\"id\":\"a\",\"name\":\"ok\"}\n{\"id\":\"b\"}\n{\"id\":\"c\",\"name\":\"fine\"}\n"
	m := &ingest.Mapping{Format: "ndjson", ID: "id", Keys: []ingest.KeyRule{{Path: "name"}}, List: exactList()}

	// Default: the keyless record fails the run, naming the record number.
	_, _, err := collect(t, m, input, ingest.Options{})
	if err == nil || !strings.Contains(err.Error(), "record 2") {
		t.Fatalf("want failure naming record 2, got %v", err)
	}
	// skip-bad tolerates it, counted.
	entries, st, err := collect(t, m, input, ingest.Options{SkipBad: 1})
	if err != nil || st.Bad != 1 || len(entries) != 2 {
		t.Fatalf("skip-bad run: %v, stats %+v, %d entries", err, st, len(entries))
	}
	// Malformed JSON goes through the same accounting.
	bad := "{\"id\":\"a\",\"name\":\"ok\"}\nnot json\n"
	if _, _, err := collect(t, m, bad, ingest.Options{}); err == nil {
		t.Fatal("malformed record accepted")
	}
	if _, st, err := collect(t, m, bad, ingest.Options{SkipBad: 5}); err != nil || st.Bad != 1 {
		t.Fatalf("malformed with skip-bad: %v %+v", err, st)
	}
}

func TestRecordBound(t *testing.T) {
	big := strings.Repeat("x", ingest.MaxRecordBytes)
	input := "{\"id\":\"a\",\"name\":\"" + big + "\"}\n"
	m := &ingest.Mapping{Format: "ndjson", ID: "id", Keys: []ingest.KeyRule{{Path: "name"}}, List: exactList()}
	if _, _, err := collect(t, m, input, ingest.Options{}); err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("oversize record accepted: %v", err)
	}
}

// The XML bound must trip DURING decode and leave the stream consumable:
// a >1MiB record is a bad record (skippable), and the FOLLOWING record
// still parses.
func TestXMLRecordBoundEarlyBail(t *testing.T) {
	big := strings.Repeat("x", ingest.MaxRecordBytes+1024)
	input := `<l><e><id>huge</id><k>` + big + `</k></e><e><id>ok</id><k>fine</k></e></l>`
	m := &ingest.Mapping{Format: "xml", Record: "e", ID: "id",
		Keys: []ingest.KeyRule{{Path: "k"}}, List: exactList()}
	if _, _, err := collect(t, m, input, ingest.Options{}); err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("oversize xml record accepted: %v", err)
	}
	entries, st, err := collect(t, m, input, ingest.Options{SkipBad: 1})
	if err != nil {
		t.Fatal(err)
	}
	if st.Bad != 1 || len(entries) != 1 || entries[0].ID != "ok" {
		t.Fatalf("stream not consumable after oversize record: %+v %+v", st, entries)
	}
}

func TestValidate(t *testing.T) {
	base := func() *ingest.Mapping {
		return &ingest.Mapping{Format: "ndjson", ID: "id", Keys: []ingest.KeyRule{{Path: "k"}}, List: exactList()}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid mapping rejected: %v", err)
	}
	for name, breakIt := range map[string]func(*ingest.Mapping){
		"bad-format":     func(m *ingest.Mapping) { m.Format = "yaml" },
		"xml-no-record":  func(m *ingest.Mapping) { m.Format = "xml" },
		"record-on-json": func(m *ingest.Mapping) { m.Record = "row" },
		"no-id":          func(m *ingest.Mapping) { m.ID = "" },
		"no-keys":        func(m *ingest.Mapping) { m.Keys = nil },
		"path-and-paths": func(m *ingest.Mapping) { m.Keys = []ingest.KeyRule{{Path: "a", Paths: []string{"b"}}} },
		"neither-path":   func(m *ingest.Mapping) { m.Keys = []ingest.KeyRule{{}} },
		"bad-list":       func(m *ingest.Mapping) { m.List.Match.Mode = "nope" },
	} {
		m := base()
		breakIt(m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
