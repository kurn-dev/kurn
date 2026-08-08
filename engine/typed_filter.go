package engine

// Typed query filters. The wire grammar is deliberately small: each
// logical name maps to a JSON string, boolean, number, or to the single
// operator {"in":[...]} whose alternatives are those same scalar types.
// Parsed filters are immutable values: their representation is private and
// every exported byte/slice accessor returns a copy.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxTypedFilterAlternatives = 64
	maxTypedFilterNumberBytes  = 256
	maxTypedFilterSignificant  = 40
	maxTypedFilterExponent     = 200
	maxTypedFilterCanonical    = 32 << 10
)

type filterScalarKind uint8

const (
	filterFalse filterScalarKind = iota
	filterTrue
	filterNumber
	filterString
)

type filterScalar struct {
	kind  filterScalarKind
	value string // decoded string or canonical fixed-decimal number
	clean bool   // string needs no JSON decoding on the payload fast path
}

type filterValue struct {
	in      bool
	scalars []filterScalar // canonical order, canonical duplicates removed
}

type typedFilterTerm struct {
	name  string
	value filterValue
}

// TypedFilter is a parsed and canonicalized typed query filter. Its zero
// value is the empty (unfiltered) expression.
type TypedFilter struct {
	terms        []typedFilterTerm // logical-name order (UTF-8 byte order)
	canonical    []byte            // minimal UTF-8 JSON object encoding
	alternatives int               // compiled scalar alternatives after dedupe
	scalarBytes  int               // canonical scalar bytes, names/operators excluded
}

// Empty reports whether the filter is the unfiltered expression.
func (f TypedFilter) Empty() bool { return len(f.terms) == 0 }

// CanonicalJSON returns the canonical minimal UTF-8 JSON encoding. The empty
// expression returns nil because successful unfiltered responses omit the
// filter member; callers that persist filters should likewise map it to NULL.
func (f TypedFilter) CanonicalJSON() []byte {
	if len(f.terms) == 0 {
		return nil
	}
	return bytes.Clone(f.canonical)
}

// Names returns the expression's logical names in deterministic UTF-8 byte
// order. It is useful at declaration/preflight boundaries without exposing
// the expression's mutable internals.
func (f TypedFilter) Names() []string {
	out := make([]string, len(f.terms))
	for i := range f.terms {
		out[i] = f.terms[i].name
	}
	return out
}

// StringTypedFilter constructs the legacy exact-string shape. It is the
// compatibility bridge used by the v0.4.0 map[string]string APIs.
func StringTypedFilter(values map[string]string) (TypedFilter, error) {
	if len(values) == 0 {
		return TypedFilter{}, nil
	}
	if len(values) > maxFilterFields {
		return TypedFilter{}, fmt.Errorf("filter has %d names, max %d", len(values), maxFilterFields)
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	f := TypedFilter{terms: make([]typedFilterTerm, 0, len(names))}
	for _, name := range names {
		if err := validateTypedFilterName(name); err != nil {
			return TypedFilter{}, err
		}
		value := values[name]
		if utf8.RuneCountInString(value) > maxFilterValueRunes {
			return TypedFilter{}, fmt.Errorf("filter value for %q exceeds %d chars", name, maxFilterValueRunes)
		}
		f.terms = append(f.terms, typedFilterTerm{name: name, value: filterValue{scalars: []filterScalar{{
			kind: filterString, value: value, clean: cleanASCII(value),
		}}}})
	}
	return finishTypedFilter(f)
}

// ParseTypedFilter parses the complete filter object with duplicate-aware
// object walks. It preserves json.Number tokens until exact arbitrary-
// precision canonicalization; float64 is never involved.
func ParseTypedFilter(raw []byte) (TypedFilter, error) {
	entries, duplicates, err := rawObject(raw, "filter")
	if err != nil {
		return TypedFilter{}, err
	}
	if len(duplicates) > 0 {
		sort.Strings(duplicates)
		return TypedFilter{}, fmt.Errorf("duplicate filter name %q (a repeated name would silently last-wins)", duplicates[0])
	}
	if len(entries) == 0 {
		return TypedFilter{}, nil
	}
	if len(entries) > maxFilterFields {
		return TypedFilter{}, fmt.Errorf("filter has %d names, max %d", len(entries), maxFilterFields)
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	f := TypedFilter{terms: make([]typedFilterTerm, 0, len(names))}
	for _, name := range names {
		if err := validateTypedFilterName(name); err != nil {
			return TypedFilter{}, err
		}
		value, err := parseTypedFilterValue(name, entries[name])
		if err != nil {
			return TypedFilter{}, err
		}
		f.terms = append(f.terms, typedFilterTerm{name: name, value: value})
	}
	return finishTypedFilter(f)
}

func validateTypedFilterName(name string) error {
	if name == "" {
		return fmt.Errorf("filter names must be non-empty")
	}
	if utf8.RuneCountInString(name) > maxFilterNameRunes {
		return fmt.Errorf("filter name %q exceeds %d chars", name, maxFilterNameRunes)
	}
	return nil
}

func parseTypedFilterValue(name string, raw json.RawMessage) (filterValue, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return filterValue{}, fmt.Errorf("filter value for %q: malformed JSON: %w", name, err)
	}
	if err := decoderEOF(dec); err != nil {
		return filterValue{}, fmt.Errorf("filter value for %q: malformed JSON: %w", name, err)
	}
	switch x := v.(type) {
	case string:
		if utf8.RuneCountInString(x) > maxFilterValueRunes {
			return filterValue{}, fmt.Errorf("filter value for %q exceeds %d chars", name, maxFilterValueRunes)
		}
		return filterValue{scalars: []filterScalar{{kind: filterString, value: x, clean: cleanASCII(x)}}}, nil
	case bool:
		kind := filterFalse
		if x {
			kind = filterTrue
		}
		return filterValue{scalars: []filterScalar{{kind: kind}}}, nil
	case json.Number:
		canon, comparable, err := canonicalNumberToken([]byte(x.String()), true)
		if err != nil {
			return filterValue{}, fmt.Errorf("filter value for %q: %w", name, err)
		}
		if !comparable {
			panic("request number accepted without a canonical form")
		}
		return filterValue{scalars: []filterScalar{{kind: filterNumber, value: canon}}}, nil
	case map[string]any:
		// Decode again through rawObject: the generic decode above establishes
		// the outer type, while the raw walk retains duplicate operator names.
		_ = x
		return parseTypedIn(name, raw)
	case nil:
		return filterValue{}, fmt.Errorf("filter value for %q must not be null", name)
	default:
		return filterValue{}, fmt.Errorf("filter value for %q must be a string, boolean, number, or {\"in\":[...]}", name)
	}
}

func parseTypedIn(name string, raw json.RawMessage) (filterValue, error) {
	entries, duplicates, err := rawObject(raw, "filter operator")
	if err != nil {
		return filterValue{}, fmt.Errorf("filter value for %q: %w", name, err)
	}
	if len(duplicates) > 0 {
		sort.Strings(duplicates)
		return filterValue{}, fmt.Errorf("filter value for %q has duplicate operator %q", name, duplicates[0])
	}
	if len(entries) != 1 {
		return filterValue{}, fmt.Errorf("filter value for %q must contain exactly one operator member", name)
	}
	inRaw, ok := entries["in"]
	if !ok {
		ops := make([]string, 0, len(entries))
		for op := range entries {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		return filterValue{}, fmt.Errorf("filter value for %q has unknown operator %q", name, ops[0])
	}
	dec := json.NewDecoder(bytes.NewReader(inRaw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return filterValue{}, fmt.Errorf("filter value for %q: in must be an array", name)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return filterValue{}, fmt.Errorf("filter value for %q: in must be an array", name)
	}
	var scalars []filterScalar
	for dec.More() {
		if len(scalars) == maxTypedFilterAlternatives {
			return filterValue{}, fmt.Errorf("filter value for %q has more than %d in alternatives", name, maxTypedFilterAlternatives)
		}
		var rawAlt json.RawMessage
		if err := dec.Decode(&rawAlt); err != nil {
			return filterValue{}, fmt.Errorf("filter value for %q: malformed in alternative: %w", name, err)
		}
		s, err := parseTypedScalar(name, rawAlt)
		if err != nil {
			return filterValue{}, err
		}
		scalars = append(scalars, s)
	}
	if _, err := dec.Token(); err != nil {
		return filterValue{}, fmt.Errorf("filter value for %q: malformed in array: %w", name, err)
	}
	if err := decoderEOF(dec); err != nil {
		return filterValue{}, fmt.Errorf("filter value for %q: malformed in array: %w", name, err)
	}
	if len(scalars) == 0 {
		return filterValue{}, fmt.Errorf("filter value for %q has an empty in set", name)
	}
	sort.Slice(scalars, func(i, j int) bool { return compareFilterScalar(scalars[i], scalars[j]) < 0 })
	out := scalars[:0]
	for _, s := range scalars {
		if len(out) == 0 || compareFilterScalar(out[len(out)-1], s) != 0 {
			out = append(out, s)
		}
	}
	// IN is a set-valued spelling of equality. Once canonical deduplication
	// leaves one member, retain semantic identity by collapsing it to the
	// direct scalar form; otherwise {"in":[x]} and x would be two persisted
	// identities for the same predicate.
	if len(out) == 1 {
		return filterValue{scalars: out}, nil
	}
	return filterValue{in: true, scalars: out}, nil
}

func parseTypedScalar(name string, raw json.RawMessage) (filterScalar, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return filterScalar{}, fmt.Errorf("filter value for %q: malformed in alternative: %w", name, err)
	}
	switch x := v.(type) {
	case string:
		if utf8.RuneCountInString(x) > maxFilterValueRunes {
			return filterScalar{}, fmt.Errorf("filter value for %q has an in string exceeding %d chars", name, maxFilterValueRunes)
		}
		return filterScalar{kind: filterString, value: x, clean: cleanASCII(x)}, nil
	case bool:
		if x {
			return filterScalar{kind: filterTrue}, nil
		}
		return filterScalar{kind: filterFalse}, nil
	case json.Number:
		canon, comparable, err := canonicalNumberToken([]byte(x.String()), true)
		if err != nil {
			return filterScalar{}, fmt.Errorf("filter value for %q: %w", name, err)
		}
		if !comparable {
			panic("request number accepted without a canonical form")
		}
		return filterScalar{kind: filterNumber, value: canon}, nil
	default:
		return filterScalar{}, fmt.Errorf("filter value for %q has a nested or null in alternative", name)
	}
}

// rawObject returns each member's exact raw value and all decoded duplicate
// names. Decoder.Token performs the same escape-aware key decoding as the
// ordinary JSON decoder, so "in" and "\u0069n" are one member.
func rawObject(raw []byte, what string) (map[string]json.RawMessage, []string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, fmt.Errorf("%s must be a JSON object: %w", what, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, fmt.Errorf("%s must be a JSON object", what)
	}
	entries := make(map[string]json.RawMessage)
	dupSet := make(map[string]struct{})
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, fmt.Errorf("malformed %s object: %w", what, err)
		}
		key, ok := kt.(string)
		if !ok {
			return nil, nil, fmt.Errorf("malformed %s object key", what)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, nil, fmt.Errorf("malformed %s value for %q: %w", what, key, err)
		}
		if _, exists := entries[key]; exists {
			dupSet[key] = struct{}{}
		} else {
			entries[key] = bytes.Clone(value)
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, fmt.Errorf("malformed %s object: %w", what, err)
	}
	if err := decoderEOF(dec); err != nil {
		return nil, nil, fmt.Errorf("malformed %s object: %w", what, err)
	}
	duplicates := make([]string, 0, len(dupSet))
	for key := range dupSet {
		duplicates = append(duplicates, key)
	}
	return entries, duplicates, nil
}

func decoderEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("trailing JSON value")
	}
	return err
}

func finishTypedFilter(f TypedFilter) (TypedFilter, error) {
	buf := make([]byte, 0, 64)
	buf = append(buf, '{')
	for i, term := range f.terms {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendJSONString(buf, term.name)
		buf = append(buf, ':')
		if term.value.in {
			buf = append(buf, '{', '"', 'i', 'n', '"', ':', '[')
		}
		for j, scalar := range term.value.scalars {
			if j > 0 {
				buf = append(buf, ',')
			}
			before := len(buf)
			buf = appendFilterScalar(buf, scalar)
			f.scalarBytes += len(buf) - before
			f.alternatives++
		}
		if term.value.in {
			buf = append(buf, ']', '}')
		}
	}
	buf = append(buf, '}')
	if len(buf) > maxTypedFilterCanonical {
		return TypedFilter{}, fmt.Errorf("canonical filter is %d bytes, max %d", len(buf), maxTypedFilterCanonical)
	}
	f.canonical = buf
	return f, nil
}

func appendFilterScalar(dst []byte, scalar filterScalar) []byte {
	switch scalar.kind {
	case filterFalse:
		return append(dst, "false"...)
	case filterTrue:
		return append(dst, "true"...)
	case filterNumber:
		return append(dst, scalar.value...)
	case filterString:
		return appendJSONString(dst, scalar.value)
	default:
		panic("unknown filter scalar kind")
	}
}

// appendJSONString is the one canonical string encoder: shortest JSON
// escapes for controls, raw valid UTF-8 for all other runes, no HTML or
// U+2028/U+2029 escaping, and never an optional solidus escape.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= utf8.RuneSelf {
			_, size := utf8.DecodeRuneInString(s[i:])
			if size == 1 {
				dst = append(dst, '\xef', '\xbf', '\xbd')
			} else {
				dst = append(dst, s[i:i+size]...)
				i += size - 1
			}
			continue
		}
		switch c {
		case '"', '\\':
			dst = append(dst, '\\', c)
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			if c < 0x20 {
				const hex = "0123456789abcdef"
				dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&15])
			} else {
				dst = append(dst, c)
			}
		}
	}
	return append(dst, '"')
}

func compareFilterScalar(a, b filterScalar) int {
	if a.kind != b.kind {
		return int(a.kind) - int(b.kind)
	}
	switch a.kind {
	case filterNumber:
		return compareCanonicalNumbers(a.value, b.value)
	case filterString:
		return strings.Compare(a.value, b.value)
	default:
		return 0
	}
}

// canonicalNumberToken validates JSON number grammar, canonicalizes an
// accepted request number, and also supports payload comparison. When
// request is false, a valid payload number outside the request's precision
// envelope returns comparable=false rather than making a different JSON type
// into an execution error.
func canonicalNumberToken(tok []byte, request bool) (canonical string, comparable bool, err error) {
	out, comparable, err := canonicalNumberTokenBytes(tok, request, nil)
	return string(out), comparable, err
}

func canonicalNumberTokenBytes(tok []byte, request bool, dst []byte) (canonical []byte, comparable bool, err error) {
	if request && len(tok) > maxTypedFilterNumberBytes {
		return nil, false, fmt.Errorf("number token is %d bytes, max %d", len(tok), maxTypedFilterNumberBytes)
	}
	if len(tok) == 0 {
		return nil, false, fmt.Errorf("invalid JSON number")
	}
	i := 0
	negative := false
	if tok[i] == '-' {
		negative = true
		i++
		if i == len(tok) {
			return nil, false, fmt.Errorf("invalid JSON number")
		}
	}
	intStart := i
	if tok[i] == '0' {
		i++
		if i < len(tok) && tok[i] >= '0' && tok[i] <= '9' {
			return nil, false, fmt.Errorf("invalid JSON number")
		}
	} else if tok[i] >= '1' && tok[i] <= '9' {
		for i < len(tok) && tok[i] >= '0' && tok[i] <= '9' {
			i++
		}
	} else {
		return nil, false, fmt.Errorf("invalid JSON number")
	}
	intEnd := i
	fracStart, fracEnd := i, i
	if i < len(tok) && tok[i] == '.' {
		i++
		fracStart = i
		for i < len(tok) && tok[i] >= '0' && tok[i] <= '9' {
			i++
		}
		if i == fracStart {
			return nil, false, fmt.Errorf("invalid JSON number")
		}
		fracEnd = i
	}
	exp := 0
	if i < len(tok) && (tok[i] == 'e' || tok[i] == 'E') {
		i++
		expNegative := false
		if i < len(tok) && (tok[i] == '+' || tok[i] == '-') {
			expNegative = tok[i] == '-'
			i++
		}
		start := i
		for i < len(tok) && tok[i] >= '0' && tok[i] <= '9' {
			if exp <= 1000000 {
				exp = exp*10 + int(tok[i]-'0')
			}
			i++
		}
		if i == start {
			return nil, false, fmt.Errorf("invalid JSON number")
		}
		if exp > 1000000 {
			exp = 1000001
		}
		if expNegative {
			exp = -exp
		}
	}
	if i != len(tok) {
		return nil, false, fmt.Errorf("invalid JSON number")
	}

	// Locate the first and last non-zero coefficient digits without ever
	// materializing a potentially huge payload token.
	first := -1
	digitOrdinal := 0
	firstOrdinal, lastOrdinal := 0, 0
	visit := func(start, end int) {
		for p := start; p < end; p++ {
			if tok[p] != '0' {
				if first < 0 {
					first, firstOrdinal = p, digitOrdinal
				}
				lastOrdinal = digitOrdinal
			}
			digitOrdinal++
		}
	}
	visit(intStart, intEnd)
	visit(fracStart, fracEnd)
	if first < 0 {
		// All zero spellings share one value. The raw exponent does not alter
		// zero's mathematical exponent.
		return append(dst, '0'), true, nil
	}
	significant := lastOrdinal - firstOrdinal + 1
	fracDigits := fracEnd - fracStart
	totalDigits := (intEnd - intStart) + fracDigits
	trailingZeros := totalDigits - 1 - lastOrdinal
	scaleExp := exp - fracDigits + trailingZeros
	mathExp := significant - 1 + scaleExp
	if significant > maxTypedFilterSignificant {
		if request {
			return nil, false, fmt.Errorf("number has %d significant digits, max %d", significant, maxTypedFilterSignificant)
		}
		return nil, false, nil
	}
	if mathExp < -maxTypedFilterExponent || mathExp > maxTypedFilterExponent {
		if request {
			return nil, false, fmt.Errorf("number mathematical exponent %d is outside [-%d, %d]", mathExp, maxTypedFilterExponent, maxTypedFilterExponent)
		}
		return nil, false, nil
	}

	var digitScratch [maxTypedFilterSignificant]byte
	digits := digitScratch[:0]
	appendRange := func(start, end int) {
		for p := start; p < end; p++ {
			ordinal := p - start
			if start == fracStart {
				ordinal += intEnd - intStart
			}
			if ordinal >= firstOrdinal && ordinal <= lastOrdinal {
				digits = append(digits, tok[p])
			}
		}
	}
	appendRange(intStart, intEnd)
	appendRange(fracStart, fracEnd)
	decimalPos := len(digits) + scaleExp
	out := dst
	if negative {
		out = append(out, '-')
	}
	switch {
	case decimalPos <= 0:
		out = append(out, '0', '.')
		for range -decimalPos {
			out = append(out, '0')
		}
		out = append(out, digits...)
	case decimalPos >= len(digits):
		out = append(out, digits...)
		for range decimalPos - len(digits) {
			out = append(out, '0')
		}
	default:
		out = append(out, digits[:decimalPos]...)
		out = append(out, '.')
		out = append(out, digits[decimalPos:]...)
	}
	return out, true, nil
}

func compareCanonicalNumbers(a, b string) int {
	if a == b {
		return 0
	}
	aNeg, bNeg := strings.HasPrefix(a, "-"), strings.HasPrefix(b, "-")
	if aNeg != bNeg {
		if aNeg {
			return -1
		}
		return 1
	}
	if aNeg {
		a, b = a[1:], b[1:]
	}
	cmp := compareCanonicalMagnitudes(a, b)
	if aNeg {
		return -cmp
	}
	return cmp
}

func compareCanonicalMagnitudes(a, b string) int {
	adot, bdot := strings.IndexByte(a, '.'), strings.IndexByte(b, '.')
	if adot < 0 {
		adot = len(a)
	}
	if bdot < 0 {
		bdot = len(b)
	}
	if adot != bdot {
		return adot - bdot
	}
	if cmp := strings.Compare(a[:adot], b[:bdot]); cmp != 0 {
		return cmp
	}
	maxFrac := len(a) - adot
	if n := len(b) - bdot; n > maxFrac {
		maxFrac = n
	}
	for i := 1; i < maxFrac; i++ {
		ac, bc := byte('0'), byte('0')
		if adot+i < len(a) {
			ac = a[adot+i]
		}
		if bdot+i < len(b) {
			bc = b[bdot+i]
		}
		if ac != bc {
			return int(ac) - int(bc)
		}
	}
	return 0
}
