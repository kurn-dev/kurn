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
		// on one line silently became one.
		if rest, _ := io.ReadAll(dec.Buffered()); len(bytes.TrimSpace(rest)) > 0 {
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

// maxCSVInputPerRecord is the memory backstop for csv.Reader, which
// materializes a whole record before returning it: without a ceiling one
// unterminated quote turns a 10 GB feed into a 10 GB allocation, and the
// package's "a 10 GB input never resides in memory" promise is only true
// for well-formed input. Deliberately far above MaxRecordBytes — a row
// between the two bounds is a bad record the run can skip, and only a row
// past this one is fatal, because a record that never ends leaves nowhere
// to resynchronize to.
const maxCSVInputPerRecord = 16 * MaxRecordBytes

// parseCSV: header row names the columns; each row becomes a
// map[string]string doc. Paths are column names verbatim.
func parseCSV(r io.Reader, m *Mapping, handle func(any, int) error) error {
	lr := &recordLimitReader{r: r, max: maxCSVInputPerRecord}
	cr := csv.NewReader(lr)
	cr.FieldsPerRecord = -1 // ragged rows surface as missing fields, not reader errors
	if m.Delimiter != "" {
		cr.Comma = []rune(m.Delimiter)[0]
	}
	header, err := cr.Read()
	if err != nil {
		return fmt.Errorf("ingest: csv header: %w", err)
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

var errCSVRecordTooBig = fmt.Errorf("record exceeds the %d-byte input ceiling (unterminated quote?)", maxCSVInputPerRecord)

// recordLimitReader caps how much INPUT one csv record may consume. The
// caller marks each record's starting input offset; the reader refuses to
// feed the parser more than max bytes past it. The parser's offset lags
// the bytes delivered by at most one bufio buffer, which is noise against
// a 16 MiB ceiling.
type recordLimitReader struct {
	r         io.Reader
	max       int64
	delivered int64
	start     int64
}

func (l *recordLimitReader) mark(off int64) { l.start = off }

func (l *recordLimitReader) Read(p []byte) (int, error) {
	if l.delivered-l.start > l.max {
		return 0, errCSVRecordTooBig
	}
	n, err := l.r.Read(p)
	l.delivered += int64(n)
	return n, err
}

// parseXML: token-walks the stream, decoding one record element subtree
// at a time (namespaces ignored — local names match). The record bound is
// enforced via the decoder's input offset across the subtree.
func parseXML(r io.Reader, m *Mapping, handle func(any, int) error) error {
	dec := xml.NewDecoder(r)
	recNo := 0
	for {
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
