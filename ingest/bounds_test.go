package ingest_test

// The record bound across all three formats. The bounds were written for
// line-structured NDJSON and only half-generalized, so each test here
// pins one place where a format enforced something different from what
// SkipBad and the package doc promise.

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/ingest"
)

func idKeyMapping(format string) *ingest.Mapping {
	m := &ingest.Mapping{
		Format: format,
		ID:     "id",
		Keys:   []ingest.KeyRule{{Path: "name"}},
		List:   exactList(),
	}
	if format == "xml" {
		m.Record = "rec"
	}
	return m
}

// An oversize NDJSON line used to abort the whole run regardless of
// -skip-bad (bufio.Scanner's ErrTooLong ends the scan), while the same
// oversize record in CSV and XML was a skippable bad record. SkipBad's
// own documentation names oversize as tolerable.
func TestNDJSONOversizeIsASkippableBadRecord(t *testing.T) {
	var b strings.Builder
	fmt.Fprintf(&b, `{"id":"a","name":"Anna"}`+"\n")
	fmt.Fprintf(&b, `{"id":"big","name":%q}`+"\n", strings.Repeat("x", 2*1024*1024))
	fmt.Fprintf(&b, `{"id":"c","name":"Clara"}`+"\n")

	entries, st, err := collect(t, idKeyMapping("ndjson"), b.String(), ingest.Options{SkipBad: 1})
	if err != nil {
		t.Fatalf("oversize line aborted a run that tolerates one bad record: %v", err)
	}
	if st.Bad != 1 {
		t.Fatalf("Bad = %d, want 1 — the oversize record must be counted, not silent", st.Bad)
	}
	// The decisive part: the reader resynchronized, so the record AFTER
	// the oversize one is still ingested.
	if len(entries) != 2 || entries[0].ID != "a" || entries[1].ID != "c" {
		t.Fatalf("entries = %+v, want a and c — the stream did not resume after the oversize line", entries)
	}
}

// Fail-by-default still holds, and the error must name the record.
func TestNDJSONOversizeFailsByDefaultWithRecordNumber(t *testing.T) {
	in := `{"id":"a","name":"Anna"}` + "\n" +
		`{"id":"big","name":"` + strings.Repeat("x", 2*1024*1024) + `"}` + "\n"
	_, _, err := collect(t, idKeyMapping("ndjson"), in, ingest.Options{})
	if err == nil {
		t.Fatal("oversize record was accepted with SkipBad=0")
	}
	if !strings.Contains(err.Error(), "record 2") {
		t.Fatalf("error does not name the offending record: %v", err)
	}
}

// A row with more fields than the header dropped the extras silently:
// the mapping cannot name a column that does not exist, so the data left
// with nothing counting it.
func TestCSVExtraFieldsAreNotDroppedSilently(t *testing.T) {
	in := "id,name\n1,Anna\n2,Bob,surprise\n3,Clara\n"
	entries, st, err := collect(t, idKeyMapping("csv"), in, ingest.Options{SkipBad: 1})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if st.Bad != 1 {
		t.Fatalf("Bad = %d, want 1 — the row with an unnamed field must be counted", st.Bad)
	}
	if len(entries) != 2 || entries[0].ID != "1" || entries[1].ID != "3" {
		t.Fatalf("entries = %+v, want rows 1 and 3", entries)
	}
}

// The counterpart: exported CSVs are full of trailing commas, and an
// extra EMPTY field carries nothing to lose. Rejecting those would fail
// real feeds for a formatting habit, so only data past the header counts.
func TestCSVTrailingCommasAreNotBadRecords(t *testing.T) {
	in := "id,name,\n1,Anna,\n2,Bob,,\n"
	entries, st, err := collect(t, idKeyMapping("csv"), in, ingest.Options{})
	if err != nil {
		t.Fatalf("trailing commas failed the run: %v", err)
	}
	if st.Bad != 0 || len(entries) != 2 {
		t.Fatalf("Bad = %d, entries = %d, want 0 and 2", st.Bad, len(entries))
	}
}

// Likewise a header whose trailing commas produce repeated EMPTY names:
// no mapping path can address them, so there is nothing to misread.
func TestCSVEmptyHeaderNamesMayRepeat(t *testing.T) {
	if _, _, err := collect(t, idKeyMapping("csv"), "id,name,,\n1,Anna,,\n", ingest.Options{}); err != nil {
		t.Fatalf("repeated empty header names failed the run: %v", err)
	}
}

// A repeated column name silently kept only the last one, so a mapping
// path naming it read a different column than its author had looked at.
func TestCSVDuplicateHeaderIsRefused(t *testing.T) {
	_, _, err := collect(t, idKeyMapping("csv"), "id,name,name\n1,Anna,Other\n", ingest.Options{SkipBad: 99})
	if err == nil {
		t.Fatal("duplicate header column was accepted")
	}
	if !strings.Contains(err.Error(), "repeats column") {
		t.Fatalf("error does not explain the duplicate: %v", err)
	}
}

// endlessQuote is a well-formed CSV opening quote followed by an infinite
// field. csv.Reader materializes a whole record before returning it, so
// without an input ceiling this is an unbounded allocation — the
// package's "a 10 GB input never resides in memory" promise applied only
// to well-formed input.
type endlessQuote struct{ n int64 }

// cap is far above the ceiling but finite, so that a build WITHOUT the
// ceiling fails this test by consuming it all rather than by exhausting
// the machine.
const endlessQuoteCap = 128 << 20

func (e *endlessQuote) Read(p []byte) (int, error) {
	const head = "id,name\n1,\"" // opens a quoted field and never closes it
	if e.n >= endlessQuoteCap {
		return 0, io.EOF
	}
	for i := range p {
		if e.n < int64(len(head)) {
			p[i] = head[e.n]
		} else {
			p[i] = 'x'
		}
		e.n++
	}
	return len(p), nil
}

func TestCSVUnterminatedRecordHitsTheInputCeiling(t *testing.T) {
	src := &endlessQuote{}
	done := make(chan error, 1)
	go func() {
		_, err := ingest.Parse(idKeyMapping("csv"), src, ingest.Options{SkipBad: 99},
			func(engine.Entry) error { return nil })
		done <- err
	}()
	err := <-done
	if err == nil {
		t.Fatal("an endless CSV record was accepted")
	}
	if !strings.Contains(err.Error(), "fatal regardless of -skip-bad") {
		t.Fatalf("error does not state the contract: %v", err)
	}
	// Bounded, not merely terminated: 16 MiB ceiling plus reader slack.
	if src.n > 24*1024*1024 {
		t.Fatalf("consumed %d bytes before stopping — the ceiling is not bounding input", src.n)
	}
	// SkipBad must NOT swallow this one: there is no next record to
	// resynchronize to, so continuing would mean guessing.
	if strings.Contains(err.Error(), "raise -skip-bad") {
		t.Fatalf("an unresynchronizable record was offered as skippable: %v", err)
	}
}

// The decided contract for the ceiling (follow-up review F5): a row past
// it is fatal even when well-formed, because the parser cannot tell it
// from an unterminated quote without unbounded reading, and a guessed
// resynchronization could silently misparse the rest of the feed. Rows
// between the 1 MiB record bound and the ceiling stay skippable.
func TestCSVWellFormedRowPastCeilingIsFatalByContract(t *testing.T) {
	in := "id,name\n1," + strings.Repeat("x", 17<<20) + "\n2,Bob\n"
	_, _, err := collect(t, idKeyMapping("csv"), in, ingest.Options{SkipBad: 5})
	if err == nil {
		t.Fatal("a row past the input ceiling was accepted")
	}
	if !strings.Contains(err.Error(), "fatal regardless of -skip-bad") {
		t.Fatalf("error does not state the contract: %v", err)
	}

	// The boundary the contract turns on: a row OVER the record bound but
	// UNDER the ceiling is an ordinary skippable bad record.
	in2 := "id,name\n1," + strings.Repeat("x", 2<<20) + "\n2,Bob\n"
	entries, st, err := collect(t, idKeyMapping("csv"), in2, ingest.Options{SkipBad: 1})
	if err != nil {
		t.Fatalf("a between-bounds row aborted the run: %v", err)
	}
	if st.Bad != 1 || len(entries) != 1 || entries[0].ID != "2" {
		t.Fatalf("Bad = %d, entries = %+v, want 1 bad and entry 2", st.Bad, entries)
	}
}

// MaxRecordBytes bounded the SOURCE record, but the store bounds the
// SERIALIZED entry. In CSV and XML a control byte is one raw byte that
// JSON escaping expands to six, so a legal source record maps to an entry
// the store refuses — caught only at writeBaseTemp, after the whole feed
// was parsed and resident, naming no record number. (NDJSON cannot show
// this: its source is already escaped, so source and serialized agree.)
func TestEscapeExpansionIsCaughtAtItsRecord(t *testing.T) {
	// 300 KiB of raw NUL: a legal CSV field well inside the 1 MiB source
	// bound, about 1.8 MiB once escaped.
	nuls := strings.Repeat("\x00", 300*1024)
	in := "id,name\n1,Anna\n2," + nuls + "\n3,Clara\n"

	_, _, err := collect(t, idKeyMapping("csv"), in, ingest.Options{})
	if err == nil {
		t.Fatal("an entry that serializes past the bound was accepted by ingest")
	}
	if !strings.Contains(err.Error(), "record 2") {
		t.Fatalf("error does not name the offending record: %v", err)
	}
	if !strings.Contains(err.Error(), "serializes to") {
		t.Fatalf("error does not explain the expansion: %v", err)
	}

	// It is a bad record, so -skip-bad applies as it does to every other
	// input-data problem, and the run continues past it.
	entries, st, err := collect(t, idKeyMapping("csv"), in, ingest.Options{SkipBad: 1})
	if err != nil {
		t.Fatalf("SkipBad did not tolerate it: %v", err)
	}
	if st.Bad != 1 || len(entries) != 2 || entries[1].ID != "3" {
		t.Fatalf("Bad = %d, entries = %+v, want 1 bad and entries 1 and 3", st.Bad, entries)
	}
}

// The bound ingest enforces must be the one the store enforces: if these
// drift, ingest either passes entries the store refuses (the bug above)
// or refuses entries the store would take.
func TestIngestBoundMatchesTheStore(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("l", exactList()); err != nil {
		t.Fatal(err)
	}
	// A key whose entry serializes just past the bound.
	e := engine.Entry{ID: "x", Keys: []string{strings.Repeat("k", ingest.MaxRecordBytes)}}
	var tooLarge *engine.EntryTooLargeError
	if err := st.Replace("l", []engine.Entry{e}); !errors.As(err, &tooLarge) {
		t.Fatalf("store accepted the oversize entry: %v", err)
	}
	// The same entry through ingest must be refused at its record, not
	// carried forward to fail at the store.
	in := "id,name\n" + e.ID + "," + e.Keys[0] + "\n"
	if _, _, err := collect(t, idKeyMapping("csv"), in, ingest.Options{}); err == nil {
		t.Fatal("ingest accepted an entry the store refuses")
	}
}

// Character data accumulated with s += string(t) per token, which is
// quadratic in the token count: many tiny CDATA sections fit inside a
// legal 1 MiB record. Allocation is the honest measure here — a wall
// clock deadline would be a flaky proxy for the same quadratic.
func TestXMLManyCharDataSectionsDoNotAllocateQuadratically(t *testing.T) {
	const sections = 40000
	var b strings.Builder
	b.WriteString(`<list><rec><id>1</id><name>`)
	for i := 0; i < sections; i++ {
		b.WriteString("<![CDATA[x]]>")
	}
	b.WriteString(`</name></rec></list>`)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	entries, _, err := collect(t, idKeyMapping("xml"), b.String(), ingest.Options{})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 1 || len(entries[0].Keys) != 1 || len(entries[0].Keys[0]) != sections {
		t.Fatalf("character data was not accumulated in full: %d entries", len(entries))
	}
	// Quadratic accumulation allocates ~sections^2/2 bytes (800 MB here);
	// linear stays within a few MB of the input.
	const ceiling = 64 << 20
	if got := after.TotalAlloc - before.TotalAlloc; got > ceiling {
		t.Fatalf("allocated %d bytes for %d sections, over the %d ceiling — accumulation is quadratic",
			got, sections, ceiling)
	}
}

var _ io.Reader = (*endlessQuote)(nil)

// The header is read before any data row and RETAINED for the whole run,
// but it skipped the record bound data rows get — a header could grow to
// the 16 MiB parser ceiling. It names the columns, so an oversize one is
// fatal (there is nothing to skip to), and the error must say so before
// any row is ingested.
func TestCSVOversizeHeaderIsFatal(t *testing.T) {
	in := "id,name," + strings.Repeat("c", 2<<20) + "\n1,Anna,x\n"
	_, _, err := collect(t, idKeyMapping("csv"), in, ingest.Options{SkipBad: 99})
	if err == nil {
		t.Fatal("a 2 MiB header was accepted")
	}
	if !strings.Contains(err.Error(), "header") || !strings.Contains(err.Error(), "record bound") {
		t.Fatalf("error does not name the header bound: %v", err)
	}

	// A wide-but-legal header stays fine.
	var b strings.Builder
	b.WriteString("id,name")
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, ",col%04d", i)
	}
	b.WriteString("\n1,Anna" + strings.Repeat(",", 500) + "\n")
	entries, _, err := collect(t, idKeyMapping("csv"), b.String(), ingest.Options{})
	if err != nil || len(entries) != 1 {
		t.Fatalf("legal wide header refused: entries=%d err=%v", len(entries), err)
	}
}

// endlessAttr opens a record tag whose attribute value never ends: the
// decoder materializes a start tag WHOLE (attributes included) before any
// offset check can see it, so only an input-level ceiling bounds it.
type endlessAttr struct{ n int64 }

func (e *endlessAttr) Read(p []byte) (int, error) {
	const head = `<root><rec id="` // an attribute value that never closes
	if e.n >= endlessQuoteCap {
		return 0, io.EOF
	}
	for i := range p {
		if e.n < int64(len(head)) {
			p[i] = head[e.n]
		} else {
			p[i] = 'x'
		}
		e.n++
	}
	return len(p), nil
}

// An XML record's opening tag was materialized before the record budget
// started: a start tag dragging a huge attribute set was allocated whole,
// outside every bound the package promises. The input ceiling now covers
// it — fatal (skipping past it would mean tokenizing without bound), with
// input consumption bounded.
func TestXMLGiantOpeningTagHitsTheInputCeiling(t *testing.T) {
	src := &endlessAttr{}
	_, err := ingest.Parse(idKeyMapping("xml"), src, ingest.Options{SkipBad: 99},
		func(engine.Entry) error { return nil })
	if err == nil {
		t.Fatal("an endless opening tag was accepted")
	}
	if !strings.Contains(err.Error(), "fatal regardless of -skip-bad") {
		t.Fatalf("error does not state the contract: %v", err)
	}
	// Bounded, not merely terminated: 16 MiB ceiling plus reader slack.
	if src.n > 24*1024*1024 {
		t.Fatalf("consumed %d bytes before stopping — the ceiling is not bounding the opening tag", src.n)
	}
}

// The two-tier contract XML now shares with CSV: a record between the
// 1 MiB record bound and the 16 MiB ceiling stays a skippable bad record,
// and the stream resumes after it.
func TestXMLBetweenBoundsRecordStaysSkippable(t *testing.T) {
	in := `<root><rec><id>a</id><name>Anna</name></rec>` +
		`<rec><id>big</id><name>` + strings.Repeat("x", 2<<20) + `</name></rec>` +
		`<rec><id>c</id><name>Clara</name></rec></root>`
	entries, st, err := collect(t, idKeyMapping("xml"), in, ingest.Options{SkipBad: 1})
	if err != nil {
		t.Fatalf("between-bounds record aborted the run: %v", err)
	}
	if st.Bad != 1 || len(entries) != 2 || entries[0].ID != "a" || entries[1].ID != "c" {
		t.Fatalf("Bad=%d entries=%+v, want 1 bad with a and c surviving", st.Bad, entries)
	}
}
