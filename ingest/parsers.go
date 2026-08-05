// The three streaming record sources. Each yields decoded records to the
// shared handler; none holds more than one record in memory.
package ingest

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// parseNDJSON: one JSON object per line, 1 MiB line bound (the engine's),
// blank lines skipped but counted for record numbering. An oversize line
// is a bad record like any other — bufio.Scanner could not express that
// (ErrTooLong ends the scan, and names no record), so the line reader
// below drains past it instead.
func parseNDJSON(r io.Reader, m *Mapping, handle func(any, int) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	recNo := 0
	for {
		line, over, err := readRecordLine(br, MaxRecordBytes)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		recNo++
		if over {
			if herr := handle(badDoc{fmt.Errorf("record exceeds the %d-byte bound", MaxRecordBytes)}, recNo); herr != nil {
				return herr
			}
			continue
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber() // keep source digits verbatim (no 1e+06 surprises)
		var doc any
		if err := dec.Decode(&doc); err != nil {
			if herr := handle(badDoc{err}, recNo); herr != nil {
				return herr
			}
			continue
		}
		// A line is ONE record. Decode stops at the first complete value, so
		// anything after it was being dropped without a word — two objects
		// on one line silently became one. The whole line is in memory, so
		// slice from the decoder's input offset: an earlier version asked
		// dec.Buffered(), which holds only the decoder's READ-AHEAD — a
		// second object beyond that buffer (say, past 10,000 spaces) sat in
		// the unread remainder and was dropped exactly as silently as
		// before the check existed.
		if rest := line[dec.InputOffset():]; len(bytes.TrimSpace(rest)) > 0 {
			if herr := handle(badDoc{fmt.Errorf("trailing content after the record on this line (one JSON object per line)")}, recNo); herr != nil {
				return herr
			}
			continue
		}
		if err := handle(doc, recNo); err != nil {
			return err
		}
	}
}

// readRecordLine reads one newline-terminated record. A line past max is
// never held: the excess is drained to the terminator, so the stream stays
// positioned at the next record and the caller can report the oversize one
// by number and carry on. io.EOF means no bytes remain — a final line with
// no trailing newline is returned normally first.
func readRecordLine(br *bufio.Reader, max int) (line []byte, over bool, err error) {
	for {
		chunk, rerr := br.ReadSlice('\n')
		switch {
		case over: // already past the bound: drain, keep nothing
		case len(line)+len(chunk) > max:
			over, line = true, nil
		default:
			line = append(line, chunk...) // chunk is valid only until the next read
		}
		switch rerr {
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(line) == 0 && !over {
				return nil, false, io.EOF
			}
			return line, over, nil
		case nil:
			return line, over, nil
		default:
			return nil, false, rerr
		}
	}
}

// badDoc marks a record that failed to decode; mapRecord turns it into a
// bad record so -skip-bad applies uniformly to malformed and unmappable
// records alike.
type badDoc struct{ err error }

// maxInputPerRecord is the memory backstop for parsers that materialize
// a whole record (csv.Reader) or token (xml.Decoder) before returning it:
// without a ceiling one unterminated quote — or one start tag dragging
// megabytes of attributes — turns a 10 GB feed into a 10 GB allocation,
// and the package's "a 10 GB input never resides in memory" promise is
// only true for well-formed input. Deliberately far above MaxRecordBytes:
// a record between the two bounds is a bad record the run can skip; only
// one past this ceiling is fatal (see the package doc — the parser cannot
// skip past it without unbounded reading, and for CSV a resync would mean
// guessing at quote state and silently misparsing the rest of the feed).
const maxInputPerRecord = 16 * MaxRecordBytes

// maxCSVColumns bounds the header's column count: every row allocates and
// iterates one slot per column, so a pathological header multiplies work
// across the whole feed. Real exports use dozens of columns; 1024 is far
// past any legitimate feed while keeping the retained header small.
const maxCSVColumns = 1024

// parseCSV: header row names the columns; each row becomes a
// map[string]string doc. Paths are column names verbatim.
func parseCSV(r io.Reader, m *Mapping, handle func(any, int) error) error {
	lr := &recordLimitReader{r: r, max: maxInputPerRecord, err: errCSVRecordTooBig}
	cr := csv.NewReader(lr)
	cr.FieldsPerRecord = -1 // ragged rows surface as missing fields, not reader errors
	if m.Delimiter != "" {
		cr.Comma = []rune(m.Delimiter)[0]
	}
	header, err := cr.Read()
	if err != nil {
		return fmt.Errorf("ingest: csv header: %w", err)
	}
	// The header is a record too, and it is RETAINED for the whole run —
	// every data row's doc is keyed by these names, and every row iterates
	// its slots. The bound is on INPUT CONSUMED, not decoded field bytes:
	// two megabytes of bare commas decode to zero name bytes while
	// allocating a column slot per comma (delimiters and quoting are
	// invisible to a field-length sum). A column-count cap closes the same
	// gap from the other side. The header names the columns, so an
	// oversize one cannot be skipped: fatal, before any row is read.
	if off := cr.InputOffset(); off > MaxRecordBytes {
		return fmt.Errorf("ingest: csv header: %d bytes of input exceeds the %d-byte record bound", off, MaxRecordBytes)
	}
	if len(header) > maxCSVColumns {
		return fmt.Errorf("ingest: csv header: %d columns exceeds the %d-column bound", len(header), maxCSVColumns)
	}
	// A repeated column name would make the doc keep only the last one,
	// so every path naming it silently reads a different column than the
	// mapping's author looked at.
	seen := make(map[string]bool, len(header))
	for _, col := range header {
		if col == "" {
			continue // trailing commas name nothing; no path can address them
		}
		if seen[col] {
			return fmt.Errorf("ingest: csv header repeats column %q — a mapping path naming it would read only the last one", col)
		}
		seen[col] = true
	}
	recNo := 0
	for {
		lr.mark(cr.InputOffset())
		row, err := cr.Read()
		if err == io.EOF {
			return nil
		}
		recNo++
		if err != nil {
			if errors.Is(err, errCSVRecordTooBig) {
				return fmt.Errorf("ingest: csv record %d: %w", recNo, err)
			}
			if herr := handle(badDoc{err}, recNo); herr != nil {
				return herr
			}
			continue
		}
		// Extra fields have no column name, so the mapping cannot reach
		// them: keeping the row would drop data with nothing to count it
		// against. Only DATA is loss — a trailing comma yields an extra
		// empty field and is waved through, since exported CSVs are full
		// of them and an empty field carries nothing to lose.
		// FieldsPerRecord is -1 by design for the opposite case (missing
		// trailing fields are legitimately absent).
		if extra := extraData(row, len(header)); extra > 0 {
			if herr := handle(badDoc{fmt.Errorf("row carries %d field(s) past the %d the header names, and an unnamed column cannot be mapped", extra, len(header))}, recNo); herr != nil {
				return herr
			}
			continue
		}
		size := 0
		doc := make(map[string]string, len(header))
		for i, col := range header {
			if i < len(row) {
				doc[col] = row[i]
				size += len(row[i])
			}
		}
		if size > MaxRecordBytes {
			if herr := handle(badDoc{fmt.Errorf("record exceeds the %d-byte bound", MaxRecordBytes)}, recNo); herr != nil {
				return herr
			}
			continue
		}
		if err := handle(doc, recNo); err != nil {
			return err
		}
	}
}

// extraData counts non-empty fields past the header's width.
func extraData(row []string, width int) int {
	n := 0
	for _, f := range row[min(len(row), width):] {
		if strings.TrimSpace(f) != "" {
			n++
		}
	}
	return n
}

var errCSVRecordTooBig = fmt.Errorf("record consumed over %d bytes of input — a huge well-formed row and an unterminated quote are indistinguishable here, so this is fatal regardless of -skip-bad; if the row is real data, it is %dx over the %d-byte record bound and needs a different transport than CSV", maxInputPerRecord, maxInputPerRecord/MaxRecordBytes, MaxRecordBytes)

// recordLimitReader caps how much INPUT one record may consume (err names
// the format-specific ceiling error). The caller marks each record's
// starting input offset; the reader refuses to feed the parser more than
// max bytes past it. The parser's offset lags the bytes delivered by at
// most one internal buffer, which is noise against a 16 MiB ceiling.
type recordLimitReader struct {
	r         io.Reader
	max       int64
	err       error
	delivered int64
	start     int64
}

func (l *recordLimitReader) mark(off int64) { l.start = off }

func (l *recordLimitReader) Read(p []byte) (int, error) {
	if l.delivered-l.start > l.max {
		return 0, l.err
	}
	n, err := l.r.Read(p)
	l.delivered += int64(n)
	return n, err
}

var errXMLInputTooBig = fmt.Errorf("record consumed over %d bytes of input — skipping past it would mean tokenizing without bound, so this is fatal regardless of -skip-bad; if the record is real data, it is %dx over the %d-byte record bound and needs to be split at the source", maxInputPerRecord, maxInputPerRecord/MaxRecordBytes, MaxRecordBytes)

// parseXML: token-walks the stream, decoding one record element subtree
// at a time (namespaces ignored — local names match). The record bound is
// enforced twice, like CSV's: the decoder's input offset across the
// subtree makes a 1-16 MiB record a SKIPPABLE bad record, and the input
// limiter is the fatal ceiling past 16 MiB. The limiter is what bounds a
// single TOKEN too: the offset checks run between tokens, and one token —
// the record's opening tag with its attributes included, or a text run —
// is materialized whole by the decoder before any offset check can see
// it. Marks refresh at every outer token, so the budget covers a record
// from BEFORE its opening tag through its subtree and any skip-drain.
func parseXML(r io.Reader, m *Mapping, handle func(any, int) error) error {
	lr := &recordLimitReader{r: r, max: maxInputPerRecord, err: errXMLInputTooBig}
	dec := xml.NewDecoder(lr)
	recNo := 0
	for {
		lr.mark(dec.InputOffset())
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("ingest: xml: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != m.Record {
			continue
		}
		recNo++
		node, err := decodeXMLNode(dec, dec.InputOffset()+MaxRecordBytes)
		if err == errRecordTooBig {
			if herr := handle(badDoc{fmt.Errorf("record exceeds the %d-byte bound", MaxRecordBytes)}, recNo); herr != nil {
				return herr
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("ingest: xml record %d: %w", recNo, err)
		}
		if err := handle(node, recNo); err != nil {
			return err
		}
	}
}

var errRecordTooBig = fmt.Errorf("record too big")

// decodeXMLNode consumes tokens up to the matching EndElement, building
// the element tree (direct character data only; child text belongs to the
// children). The bound trips DURING decode (review finding): a hostile
// single record is abandoned at the limit — the partial tree is dropped
// and the remaining tokens are drained to the record's end, so it never
// fully materializes and the stream stays consumable for the next record.
func decodeXMLNode(dec *xml.Decoder, limit int64) (*xnode, error) {
	root := &xnode{children: map[string][]*xnode{}}
	stack := []*xnode{root}
	for {
		if dec.InputOffset() > limit {
			for depth := len(stack); depth > 0; {
				tok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				switch tok.(type) {
				case xml.StartElement:
					depth++
				case xml.EndElement:
					depth--
				}
			}
			return nil, errRecordTooBig
		}
		tok, err := dec.Token()
		if err != nil {
			return nil, err // EOF inside a record is malformed
		}
		top := stack[len(stack)-1]
		switch t := tok.(type) {
		case xml.StartElement:
			child := &xnode{children: map[string][]*xnode{}}
			top.children[t.Name.Local] = append(top.children[t.Name.Local], child)
			stack = append(stack, child)
		case xml.CharData:
			// Appending to the string would be O(n^2) in the token count:
			// ~80k one-char CDATA sections fit inside a legal 1 MiB record
			// and cost seconds of CPU each. Accumulate, materialize once.
			top.buf = append(top.buf, t...)
		case xml.EndElement:
			top.text, top.buf = string(top.buf), nil
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return root, nil
			}
		}
	}
}
