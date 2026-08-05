package ingest_test

// Silent-loss corners the parsers used to wave through.

import (
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/ingest"
)

// Every spreadsheet that exports "CSV UTF-8" writes a BOM, and it lands on
// the first header cell: the column becomes a BOM-prefixed "id", so a mapping path
// naming id misses on every row and the whole feed fails as "missing id".
func TestBOMIsStripped(t *testing.T) {
	const bom = "\ufeff"
	for _, tc := range []struct{ name, in, format string }{
		{"csv", bom + "id,name\n1,Anna\n", "csv"},
		{"ndjson", bom + `{"id":"1","name":"Anna"}` + "\n", "ndjson"},
		{"xml", bom + "<list><rec><id>1</id><name>Anna</name></rec></list>", "xml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, st, err := collect(t, idKeyMapping(tc.format), tc.in, ingest.Options{})
			if err != nil {
				t.Fatalf("a byte-order mark failed the feed: %v", err)
			}
			if st.Bad != 0 || len(entries) != 1 || entries[0].ID != "1" {
				t.Fatalf("Bad = %d, entries = %+v, want one clean entry", st.Bad, entries)
			}
		})
	}
}

// A line is one record: Decode stops at the first complete value, so a
// second object on the same line was dropped without a word.
func TestNDJSONTrailingContentIsABadRecord(t *testing.T) {
	in := `{"id":"1","name":"Anna"}` + "\n" +
		`{"id":"2","name":"Bob"} {"id":"3","name":"Clara"}` + "\n"

	_, _, err := collect(t, idKeyMapping("ndjson"), in, ingest.Options{})
	if err == nil {
		t.Fatal("two records on one line were accepted as one")
	}
	if !strings.Contains(err.Error(), "record 2") {
		t.Fatalf("error does not name the offending line: %v", err)
	}

	entries, st, err := collect(t, idKeyMapping("ndjson"), in, ingest.Options{SkipBad: 1})
	if err != nil {
		t.Fatalf("SkipBad did not tolerate it: %v", err)
	}
	if st.Bad != 1 || len(entries) != 1 || entries[0].ID != "1" {
		t.Fatalf("Bad = %d, entries = %+v, want 1 bad and only entry 1", st.Bad, entries)
	}

	// Whitespace after the record is not content.
	if _, st, err := collect(t, idKeyMapping("ndjson"), `{"id":"1","name":"Anna"}   `+"\n", ingest.Options{}); err != nil || st.Bad != 0 {
		t.Fatalf("trailing whitespace treated as content: bad=%d err=%v", st.Bad, err)
	}
}
