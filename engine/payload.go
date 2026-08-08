package engine

// Byte-level payload scanning for query-time filters. Dot-path traversal
// mirrors the ingest mapping's semantics (split on dots, arrays
// auto-descend, any reached value may match); equality is decoded and
// type-exact for strings, booleans, and arbitrary-precision canonical JSON
// numbers. An escaped string spelling ("SD\u004e") equals its decoded form.
// One strict pass: matches are recorded, never
// early-exited, so a malformed tail can never hide behind a hit. Everything
// is read-only over the payload's backing array; the only allocations are
// slow-path string decodes.
//
// The scanner exists because encoding/json decode-per-candidate is ~20x
// more expensive and allocation-heavy at query volumes (measured in the
// iteration-5 study): filters evaluate per score-qualifying candidate,
// before the top-K cut.

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

// payloadScan is a cursor over one payload's bytes, plus the compiled
// compare context for this term.
type payloadScan struct {
	b         []byte
	i         int
	want      string
	wantClean bool // want needs no JSON escaping
	segsClean bool // every path segment needs no JSON escaping
}

// scanError is returned for syntactically malformed or truncated JSON.
// Filtered queries surface it as an error rather than treating the entry
// as a non-match — a dropped candidate could otherwise manufacture a
// clear result.
type scanError struct{ msg string }

func (e *scanError) Error() string { return "engine: malformed payload JSON: " + e.msg }

func (s *payloadScan) ws() {
	for s.i < len(s.b) {
		switch s.b[s.i] {
		case ' ', '\t', '\n', '\r':
			s.i++
		default:
			return
		}
	}
}

// rawString consumes a JSON string token and returns its inner span
// (between the quotes, escapes undecoded). Invalid escapes, raw control
// characters (< 0x20), and truncation are rejected.
func (s *payloadScan) rawString() (int, int, bool) {
	if s.i >= len(s.b) || s.b[s.i] != '"' {
		return 0, 0, false
	}
	s.i++
	start := s.i
	for {
		rest := s.b[s.i:]
		q := bytes.IndexByte(rest, '"')
		bs := bytes.IndexByte(rest, '\\')
		if q < 0 && bs < 0 {
			return 0, 0, false
		}
		if bs >= 0 && (q < 0 || bs < q) {
			if hasControl(rest[:bs]) {
				return 0, 0, false
			}
			// validate the escape at s.i+bs
			e := s.i + bs
			if e+1 >= len(s.b) {
				return 0, 0, false
			}
			switch s.b[e+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				s.i = e + 2
			case 'u':
				if e+6 > len(s.b) {
					return 0, 0, false
				}
				for _, h := range s.b[e+2 : e+6] {
					if !isHex(h) {
						return 0, 0, false
					}
				}
				s.i = e + 6
			default:
				return 0, 0, false
			}
			continue
		}
		if hasControl(rest[:q]) {
			return 0, 0, false
		}
		end := s.i + q
		s.i = end + 1
		return start, end, true
	}
}

func hasControl(b []byte) bool {
	for _, c := range b {
		if c < 0x20 {
			return true
		}
	}
	return false
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// skipValue consumes and fully validates one value of any type —
// separator grammar included (encoding/json strictness). Payload size
// bounds recursion depth the same way it bounds encoding/json's.
func (s *payloadScan) skipValue() error {
	s.ws()
	if s.i >= len(s.b) {
		return &scanError{"truncated value"}
	}
	switch c := s.b[s.i]; {
	case c == '"':
		if _, _, ok := s.rawString(); !ok {
			return &scanError{"bad string token"}
		}
		return nil
	case c == '{':
		return s.skipObject()
	case c == '[':
		return s.skipArray()
	default: // number, true, false, null: runs to a delimiter
		_, _, err := s.rawScalar()
		return err
	}
}

// rawScalar consumes and validates one non-string JSON scalar token.
func (s *payloadScan) rawScalar() (int, int, error) {
	start := s.i
	for s.i < len(s.b) {
		switch s.b[s.i] {
		case ',', '}', ']', '{', '[', '"', ' ', '\t', '\n', '\r':
			goto tokend
		}
		s.i++
	}
tokend:
	if !validScalarToken(s.b[start:s.i]) {
		return 0, 0, &scanError{"invalid scalar"}
	}
	return start, s.i, nil
}

func (s *payloadScan) skipObject() error {
	s.i++ // consume '{'
	s.ws()
	if s.i < len(s.b) && s.b[s.i] == '}' {
		s.i++
		return nil
	}
	for {
		s.ws()
		if _, _, ok := s.rawString(); !ok {
			return &scanError{"bad object key"}
		}
		s.ws()
		if s.i >= len(s.b) || s.b[s.i] != ':' {
			return &scanError{"missing ':'"}
		}
		s.i++
		if err := s.skipValue(); err != nil {
			return err
		}
		s.ws()
		if s.i >= len(s.b) {
			return &scanError{"truncated object"}
		}
		if s.b[s.i] == ',' {
			s.i++
			continue
		}
		if s.b[s.i] == '}' {
			s.i++
			return nil
		}
		return &scanError{"bad object separator"}
	}
}

func (s *payloadScan) skipArray() error {
	s.i++ // consume '['
	s.ws()
	if s.i < len(s.b) && s.b[s.i] == ']' {
		s.i++
		return nil
	}
	for {
		if err := s.skipValue(); err != nil {
			return err
		}
		s.ws()
		if s.i >= len(s.b) {
			return &scanError{"truncated array"}
		}
		if s.b[s.i] == ',' {
			s.i++
			continue
		}
		if s.b[s.i] == ']' {
			s.i++
			return nil
		}
		return &scanError{"bad array separator"}
	}
}

// validScalarToken enforces the JSON number/true/false/null grammar so
// bare words and junk are malformed, matching encoding/json's strictness.
func validScalarToken(tok []byte) bool {
	switch string(tok) {
	case "true", "false", "null":
		return true
	}
	// -?(0|[1-9]\d*)(\.\d+)?([eE][+-]?\d+)?
	i := 0
	if i < len(tok) && tok[i] == '-' {
		i++
	}
	digits := func() bool {
		start := i
		for i < len(tok) && tok[i] >= '0' && tok[i] <= '9' {
			i++
		}
		return i > start
	}
	switch {
	case i >= len(tok):
		return false
	case tok[i] == '0':
		i++
	case tok[i] >= '1' && tok[i] <= '9':
		digits()
	default:
		return false
	}
	if i < len(tok) && tok[i] == '.' {
		i++
		if !digits() {
			return false
		}
	}
	if i < len(tok) && (tok[i] == 'e' || tok[i] == 'E') {
		i++
		if i < len(tok) && (tok[i] == '+' || tok[i] == '-') {
			i++
		}
		if !digits() {
			return false
		}
	}
	return i == len(tok)
}

// decodedStringEq compares a JSON string token with a plain Go string.
// inner is the span between the quotes, token includes them. Fast path:
// escape-free, valid-UTF-8 inner, and a want that needs no JSON escaping
// (precomputed at filter-compile time). Everything else decodes first —
// including invalid UTF-8, which json.Unmarshal coerces to U+FFFD.
func decodedStringEq(inner, token []byte, want string, wantClean bool) bool {
	if bytes.IndexByte(inner, '\\') < 0 && wantClean && utf8.Valid(inner) {
		return bytes.Equal(inner, []byte(want))
	}
	var got string
	if err := json.Unmarshal(token, &got); err != nil {
		return false
	}
	return got == want
}

func cleanASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// filterNode is one node of a compiled filter's path trie: children keyed
// by path segment, and the terminal terms ending exactly at this node.
type filterNode struct {
	children map[string]*filterNode
	clean    map[string]bool // per child segment: needs no JSON escaping
	terms    []terminalTerm
}

// terminalTerm is one AND term ending at this node.
type terminalTerm struct {
	bit   uint8 // this term's bit in the match mask
	value filterValue
}

// child returns the child whose segment matches the raw key bytes (decoded
// compare), or nil. At most one child can match: segments are unique.
func (n *filterNode) child(inner, token []byte) *filterNode {
	for seg, c := range n.children {
		if decodedStringEq(inner, token, seg, n.clean[seg]) {
			return c
		}
	}
	return nil
}

// walkValue evaluates one value against a trie node: objects descend into
// matching children, arrays auto-descend (same node), strings fire the
// node's terminals, everything else is a valid non-match. Matches are
// recorded, never early-exited — the whole document is validated either
// way, so a corrupt tail cannot hide behind a hit.
func (s *payloadScan) walkValue(n *filterNode, bits *uint8) error {
	s.ws()
	if s.i >= len(s.b) {
		return &scanError{"empty/truncated value"}
	}
	switch s.b[s.i] {
	case '{':
		return s.walkObject(n, bits)
	case '[':
		return s.walkArray(n, bits)
	case '"':
		vs, ve, ok := s.rawString()
		if !ok {
			return &scanError{"bad string token"}
		}
		for _, t := range n.terms {
			if *bits&t.bit == 0 && t.value.matchesString(s.b[vs:ve], s.b[vs-1:ve+1]) {
				*bits |= t.bit
			}
		}
		return nil
	default:
		start, end, err := s.rawScalar()
		if err != nil {
			return err
		}
		for _, t := range n.terms {
			if *bits&t.bit == 0 && t.value.matchesScalar(s.b[start:end]) {
				*bits |= t.bit
			}
		}
		return nil
	}
}

func (v filterValue) matchesString(inner, token []byte) bool {
	for _, scalar := range v.scalars {
		if scalar.kind == filterString && decodedStringEq(inner, token, scalar.value, scalar.clean) {
			return true
		}
	}
	return false
}

func (v filterValue) matchesScalar(token []byte) bool {
	switch string(token) {
	case "false":
		for _, scalar := range v.scalars {
			if scalar.kind == filterFalse {
				return true
			}
		}
		return false
	case "true":
		for _, scalar := range v.scalars {
			if scalar.kind == filterTrue {
				return true
			}
		}
		return false
	case "null":
		return false
	default:
		var scratch [256]byte
		canonical, comparable, err := canonicalNumberTokenBytes(token, false, scratch[:0])
		if err != nil || !comparable {
			return false
		}
		for _, scalar := range v.scalars {
			if scalar.kind == filterNumber && bytes.Equal([]byte(scalar.value), canonical) {
				return true
			}
		}
		return false
	}
}

func (s *payloadScan) walkObject(n *filterNode, bits *uint8) error {
	s.i++ // consume '{'
	s.ws()
	if s.i < len(s.b) && s.b[s.i] == '}' {
		s.i++
		return nil
	}
	for {
		s.ws()
		ks, ke, ok := s.rawString()
		if !ok {
			return &scanError{"bad object key"}
		}
		s.ws()
		if s.i >= len(s.b) || s.b[s.i] != ':' {
			return &scanError{"missing ':'"}
		}
		s.i++
		if c := n.child(s.b[ks:ke], s.b[ks-1:ke+1]); c != nil {
			if err := s.walkValue(c, bits); err != nil {
				return err
			}
		} else if err := s.skipValue(); err != nil {
			return err
		}
		s.ws()
		if s.i >= len(s.b) {
			return &scanError{"truncated object"}
		}
		if s.b[s.i] == ',' {
			s.i++
			continue
		}
		if s.b[s.i] == '}' {
			s.i++
			return nil
		}
		return &scanError{"bad object separator"}
	}
}

func (s *payloadScan) walkArray(n *filterNode, bits *uint8) error {
	s.i++ // consume '['
	s.ws()
	if s.i < len(s.b) && s.b[s.i] == ']' {
		s.i++
		return nil
	}
	for {
		if err := s.walkValue(n, bits); err != nil {
			return err
		}
		s.ws()
		if s.i >= len(s.b) {
			return &scanError{"truncated array"}
		}
		if s.b[s.i] == ',' {
			s.i++
			continue
		}
		if s.b[s.i] == ']' {
			s.i++
			return nil
		}
		return &scanError{"bad array separator"}
	}
}

// pathMatch reports whether any value reached by segs in payload is a
// JSON string whose decoded form equals want. Used by the scanner tests;
// the compiled-filter path walks a prebuilt trie (one pass, all terms).
func pathMatch(payload []byte, segs []string, want string, wantClean, segsClean bool) (bool, error) {
	if len(segs) == 0 {
		return false, nil
	}
	root := &filterNode{}
	n := root
	for _, seg := range segs {
		if n.children == nil {
			n.children = map[string]*filterNode{}
			n.clean = map[string]bool{}
		}
		child, ok := n.children[seg]
		if !ok {
			child = &filterNode{}
			n.children[seg] = child
			n.clean[seg] = cleanASCII(seg)
		}
		n = child
	}
	n.terms = append(n.terms, terminalTerm{bit: 1, value: filterValue{scalars: []filterScalar{{
		kind: filterString, value: want, clean: wantClean,
	}}}})
	s := &payloadScan{b: payload}
	var bits uint8
	if err := s.walkValue(root, &bits); err != nil {
		return false, err
	}
	s.ws()
	if s.i != len(s.b) {
		return false, &scanError{"trailing data"}
	}
	return bits != 0, nil
}
