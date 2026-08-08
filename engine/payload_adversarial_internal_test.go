package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

const (
	typedPayloadAdversarialSeed      = int64(0x51c2a55)
	typedPayloadAdversarialGenerated = 4096
	typedPayloadAdversarialCount     = 4114
	typedPayloadAdversarialDigest    = "2d3b6ac515ede7f156e975d9069779b177d9924c80e1ab8f876c98d2f820b9d7"
)

// refJSON retains object members in source order, including duplicates. It is
// decoded entirely through encoding/json tokens before evaluation, making a
// malformed tail an error even when an earlier term already matched.
type refJSON struct {
	kind byte
	str  string
	num  string
	b    bool
	obj  []refMember
	arr  []refJSON
}

type refMember struct {
	name  string
	value refJSON
}

func decodeRefJSON(raw []byte) (refJSON, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeRefValue(dec)
	if err != nil {
		return refJSON{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return refJSON{}, fmt.Errorf("trailing JSON value")
		}
		return refJSON{}, err
	}
	return v, nil
}

func decodeRefValue(dec *json.Decoder) (refJSON, error) {
	tok, err := dec.Token()
	if err != nil {
		return refJSON{}, err
	}
	switch x := tok.(type) {
	case json.Delim:
		switch x {
		case '{':
			v := refJSON{kind: 'o'}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return refJSON{}, err
				}
				name, ok := key.(string)
				if !ok {
					return refJSON{}, fmt.Errorf("non-string object key")
				}
				child, err := decodeRefValue(dec)
				if err != nil {
					return refJSON{}, err
				}
				v.obj = append(v.obj, refMember{name: name, value: child})
			}
			if end, err := dec.Token(); err != nil || end != json.Delim('}') {
				return refJSON{}, fmt.Errorf("bad object close: %v", err)
			}
			return v, nil
		case '[':
			v := refJSON{kind: 'a'}
			for dec.More() {
				child, err := decodeRefValue(dec)
				if err != nil {
					return refJSON{}, err
				}
				v.arr = append(v.arr, child)
			}
			if end, err := dec.Token(); err != nil || end != json.Delim(']') {
				return refJSON{}, fmt.Errorf("bad array close: %v", err)
			}
			return v, nil
		default:
			return refJSON{}, fmt.Errorf("unexpected delimiter %q", x)
		}
	case string:
		return refJSON{kind: 's', str: x}, nil
	case bool:
		return refJSON{kind: 'b', b: x}, nil
	case json.Number:
		return refJSON{kind: 'n', num: x.String()}, nil
	case nil:
		return refJSON{kind: '0'}, nil
	default:
		return refJSON{}, fmt.Errorf("unexpected token %T", tok)
	}
}

func refFilterEval(raw []byte, cf *compiledFilter) (bool, error) {
	v, err := decodeRefJSON(raw)
	if err != nil {
		return false, err
	}
	for _, term := range cf.terms {
		if !refPathMatches(v, term.segs, term.value) {
			return false, nil
		}
	}
	return true, nil
}

func refPathMatches(v refJSON, segs []string, want filterValue) bool {
	if len(segs) == 0 {
		if v.kind == 'a' {
			for _, child := range v.arr {
				if refPathMatches(child, nil, want) {
					return true
				}
			}
			return false
		}
		return refScalarMatches(v, want)
	}
	switch v.kind {
	case 'a':
		for _, child := range v.arr {
			if refPathMatches(child, segs, want) {
				return true
			}
		}
	case 'o':
		for _, member := range v.obj {
			if member.name == segs[0] && refPathMatches(member.value, segs[1:], want) {
				return true
			}
		}
	}
	return false
}

func refScalarMatches(v refJSON, want filterValue) bool {
	for _, scalar := range want.scalars {
		switch {
		case v.kind == 's' && scalar.kind == filterString && v.str == scalar.value:
			return true
		case v.kind == 'b' && !v.b && scalar.kind == filterFalse:
			return true
		case v.kind == 'b' && v.b && scalar.kind == filterTrue:
			return true
		case v.kind == 'n' && scalar.kind == filterNumber && refNumberEqual(v.num, scalar.value):
			return true
		}
	}
	return false
}

// refNumberEqual uses math/big for value equality and independently applies
// the request-number comparability envelope to payload numbers. Valid payload
// numbers outside that envelope are other values, not query errors.
func refNumberEqual(payload, canonicalWant string) bool {
	if !refPayloadNumberComparable(payload) {
		return false
	}
	a, b := new(big.Rat), new(big.Rat)
	if _, ok := a.SetString(payload); !ok {
		return false
	}
	if _, ok := b.SetString(canonicalWant); !ok {
		return false
	}
	return a.Cmp(b) == 0
}

func refPayloadNumberComparable(tok string) bool {
	s := strings.TrimPrefix(tok, "-")
	mantissa, exponent := s, "0"
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mantissa, exponent = s[:i], s[i+1:]
	}
	integer, fraction := mantissa, ""
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		integer, fraction = mantissa[:i], mantissa[i+1:]
	}
	digits := integer + fraction
	nonzero := strings.TrimLeft(digits, "0")
	if nonzero == "" {
		return true
	}
	nonzero = strings.TrimRight(nonzero, "0")
	if len(nonzero) > maxTypedFilterSignificant {
		return false
	}
	if len(strings.TrimLeft(strings.TrimPrefix(strings.TrimPrefix(exponent, "+"), "-"), "0")) > 6 {
		return false
	}
	exp, err := strconv.Atoi(exponent)
	if err != nil {
		return false
	}
	trailing := len(digits) - len(strings.TrimRight(digits, "0"))
	mathExp := len(nonzero) - 1 + exp - len(fraction) + trailing
	return mathExp >= -maxTypedFilterExponent && mathExp <= maxTypedFilterExponent
}

type payloadAdversarialCase struct {
	filter  string
	payload []byte
}

func payloadAdversarialCases(t *testing.T) []payloadAdversarialCase {
	t.Helper()
	filters := []string{
		`{"program":"SDN"}`,
		`{"program":{"in":["SDN","x","\u0000"]}}`,
		`{"active":true}`,
		`{"amount":{"in":[false,true,-0,1,1.0,1e0,1e-200,1e200]}}`,
		`{"nested":"x","listed":{"in":["a",2,false]}}`,
		`{"program":"S\u0044N","active":{"in":[true,false]},"amount":1e0}`,
	}
	cases := []payloadAdversarialCase{
		{filters[0], []byte(`{"program":"SDN"}`)},
		{filters[0], []byte(`{"pro\u0067ram":"S\u0044N"}`)},
		{filters[1], []byte(`{"program":"\u0000"}`)},
		{filters[2], []byte(`{"active":true}`)},
		{filters[3], []byte(`{"amount":1.00}`)},
		{filters[3], []byte(`{"amount":1e0}`)},
		{filters[3], []byte(`{"amount":1e201}`)},
		{filters[4], []byte(`{"meta":{"program":"x"},"items":[{"value":2}]}`)},
		{filters[4], []byte(`{"meta":[{"program":"x"}],"items":[[{"value":false}]]}`)},
		{filters[0], []byte(`{"program":{"nested":"SDN"}}`)},
		{filters[0], []byte(`{"program":null}`)},
		{filters[0], []byte(`{"program":"miss","program":"SDN"}`)},
		{filters[0], []byte(`{"program":"SDN"}{"broken":`)},
		{filters[0], []byte(`{"program":"SDN"} true`)},
		{filters[0], []byte(`{"program":"SDN"`)},
		{filters[0], []byte(`{"program":"SDN\x"}`)},
		{filters[0], append([]byte(`{"program":"`), append([]byte{0xff}, []byte(`"}`)...)...)},
		{filters[5], []byte(`{"program":"SDN","active":false,"amount":1.000}`)},
	}

	r := rand.New(rand.NewSource(typedPayloadAdversarialSeed))
	for i := 0; i < typedPayloadAdversarialGenerated; i++ {
		doc := map[string]any{
			"program": adversarialScalar(r),
			"active":  r.Intn(2) == 0,
			"amount":  adversarialNumber(r),
			"meta":    map[string]any{"program": adversarialScalar(r), "noise": adversarialValue(r, 0)},
			"items": []any{
				map[string]any{"value": adversarialScalar(r)},
				[]any{map[string]any{"value": adversarialScalar(r)}},
			},
			"noise": adversarialValue(r, 0),
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		switch i % 11 {
		case 0:
			raw = raw[:len(raw)-1]
		case 1:
			raw = append(raw, []byte(`{"malformed":`)...)
		case 2:
			raw = append(raw, []byte(` true`)...)
		case 3:
			raw = []byte(`{"program":"SDN","tail":[1,]}`)
		case 4:
			raw = []byte(`{"pro\u0067ram":"S\u0044N","active":true,"amount":1e0}`)
		}
		cases = append(cases, payloadAdversarialCase{filter: filters[i%len(filters)], payload: raw})
	}
	return cases
}

func adversarialNumber(r *rand.Rand) json.Number {
	values := []json.Number{"0", "-0.00e99", "1", "1.0", "1e0", "2", "-1", "1e-200", "1e200", "1e201", "12345678901234567890123456789012345678901"}
	return values[r.Intn(len(values))]
}

func adversarialScalar(r *rand.Rand) any {
	values := []any{"SDN", "S\x00DN", "x", "a", "é", "quote\"slash\\", true, false, nil, adversarialNumber(r)}
	return values[r.Intn(len(values))]
}

func adversarialValue(r *rand.Rand, depth int) any {
	if depth >= 3 {
		return adversarialScalar(r)
	}
	switch r.Intn(5) {
	case 0:
		return adversarialScalar(r)
	case 1:
		return []any{adversarialValue(r, depth+1), adversarialValue(r, depth+1)}
	case 2:
		return map[string]any{"program": adversarialValue(r, depth+1), "k": adversarialValue(r, depth+1)}
	case 3:
		return map[string]any{}
	default:
		return []any{}
	}
}

func hashAdversarialCase(h interface{ Write([]byte) (int, error) }, c payloadAdversarialCase) {
	var size [8]byte
	for _, b := range [][]byte{[]byte(c.filter), c.payload} {
		binary.LittleEndian.PutUint64(size[:], uint64(len(b)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(b)
	}
}

// This fixed-seed differential is the permanent H2 assurance: complete
// encoding/json validation and independent typed/path evaluation must agree
// with the allocation-free one-pass scanner on every valid, malformed, and
// trailing-data case.
func TestTypedPayloadAdversarialDifferential(t *testing.T) {
	cases := payloadAdversarialCases(t)
	if len(cases) != typedPayloadAdversarialCount {
		t.Fatalf("case count = %d, want %d", len(cases), typedPayloadAdversarialCount)
	}
	l, err := NewList("adversarial", ListConfig{
		Analyzer: AnalyzerConfig{Steps: []string{"lowercase"}},
		Match:    MatchConfig{Mode: "exact"},
		Filterable: []FilterField{
			{Name: "active", Path: "active"},
			{Name: "amount", Path: "amount"},
			{Name: "listed", Path: "items.value"},
			{Name: "nested", Path: "meta.program"},
			{Name: "program", Path: "program"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled := map[string]*compiledFilter{}
	h := sha256.New()
	for i, c := range cases {
		hashAdversarialCase(h, c)
		cf := compiled[c.filter]
		if cf == nil {
			f, err := ParseTypedFilter([]byte(c.filter))
			if err != nil {
				t.Fatalf("case %d filter: %v", i, err)
			}
			cf, err = l.compileTypedFilter(f)
			if err != nil {
				t.Fatalf("case %d compile: %v", i, err)
			}
			compiled[c.filter] = cf
		}
		got, gotErr := cf.eval(c.payload)
		want, wantErr := refFilterEval(c.payload, cf)
		if (gotErr != nil) != (wantErr != nil) {
			t.Fatalf("case %d error mismatch\nfilter: %s\npayload: %q\nscanner: %v\nreference: %v", i, c.filter, c.payload, gotErr, wantErr)
		}
		if gotErr == nil && got != want {
			t.Fatalf("case %d result mismatch\nfilter: %s\npayload: %q\nscanner: %v\nreference: %v", i, c.filter, c.payload, got, want)
		}
	}
	gotDigest := hex.EncodeToString(h.Sum(nil))
	if gotDigest != typedPayloadAdversarialDigest {
		t.Fatalf("case digest = %s, want %s", gotDigest, typedPayloadAdversarialDigest)
	}
}

func TestTypedFilterCallerMutationAndRepeatedExecution(t *testing.T) {
	raw := []byte(`{"program":{"in":["SDN",1,true]}}`)
	f, err := ParseTypedFilter(raw)
	if err != nil {
		t.Fatal(err)
	}
	wantEcho := string(f.CanonicalJSON())
	for i := range raw {
		raw[i] = 'X'
	}
	echo := f.CanonicalJSON()
	for i := range echo {
		echo[i] = 'X'
	}
	if got := string(f.CanonicalJSON()); got != wantEcho {
		t.Fatalf("caller mutation changed parsed filter: %s != %s", got, wantEcho)
	}

	l, err := NewList("repeat", ListConfig{
		Analyzer:   AnalyzerConfig{Steps: []string{"lowercase"}},
		Match:      MatchConfig{Mode: "exact"},
		Filterable: []FilterField{{Name: "program", Path: "program"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]Entry{{ID: "hit", Keys: []string{"key"}, Payload: json.RawMessage(`{"program":"SDN"}`)}}); err != nil {
		t.Fatal(err)
	}
	p, err := l.PrepareTypedFilteredQuery("key", QueryOpts{}, f)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		cands, _, stats, err := p.ExecuteStats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(cands) != 1 || cands[0].EntryID != "hit" || stats.Evaluated != 1 || stats.Rejected != 0 {
			t.Fatalf("execution %d drifted: candidates=%+v stats=%+v", i, cands, stats)
		}
		applied := p.AppliedFilterJSON()
		applied[0] = 'X'
		if got := string(p.AppliedFilterJSON()); got != wantEcho {
			t.Fatalf("execution %d echo mutated: %s != %s", i, got, wantEcho)
		}
	}
}
