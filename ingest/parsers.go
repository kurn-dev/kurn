// The three streaming record sources. Each yields decoded records to the
// shared handler; none holds more than one record in memory.
package ingest

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
)

// parseNDJSON: one JSON object per line, 1 MiB line bound (the engine's),
// blank lines skipped but counted for record numbering.
func parseNDJSON(r io.Reader, m *Mapping, handle func(any, int) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), MaxRecordBytes)
	recNo := 0
	for sc.Scan() {
		recNo++
		line := bytes.TrimSpace(sc.Bytes())
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
		if err := handle(doc, recNo); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		if err == bufio.ErrTooLong {
			return fmt.Errorf("ingest: record exceeds the %d-byte bound", MaxRecordBytes)
		}
		return err
	}
	return nil
}

// badDoc marks a record that failed to decode; mapRecord turns it into a
// bad record so -skip-bad applies uniformly to malformed and unmappable
// records alike.
type badDoc struct{ err error }

// parseCSV: header row names the columns; each row becomes a
// map[string]string doc. Paths are column names verbatim.
func parseCSV(r io.Reader, m *Mapping, handle func(any, int) error) error {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // ragged rows surface as missing fields, not reader errors
	if m.Delimiter != "" {
		cr.Comma = []rune(m.Delimiter)[0]
	}
	header, err := cr.Read()
	if err != nil {
		return fmt.Errorf("ingest: csv header: %w", err)
	}
	recNo := 0
	for {
		row, err := cr.Read()
		if err == io.EOF {
			return nil
		}
		recNo++
		if err != nil {
			if herr := handle(badDoc{err}, recNo); herr != nil {
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
			top.text += string(t)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return root, nil
			}
		}
	}
}
