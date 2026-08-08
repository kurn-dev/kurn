package engine_test

// Self-consistency check for the shared canonicalization fixture corpus
// (engine/testdata/filter_canonical_corpus.json, the drift guard the
// v0.5.0 C2 scope requires across Go, HTTP, registration, and
// PostgreSQL). The reference implementation here is test-only and
// deliberately independent of the engine's canonicalizer: two
// implementations agreeing on the corpus is the guard. Rules under
// test: canonical fixed-decimal numbers (no exponent, one integer zero
// below one, no redundant zeros, -0 -> 0; <=40 significant digits,
// mathematical exponent within +-200, token <=256 bytes), IN as a
// deduplicated set sorted false < true < numbers by value < strings by
// UTF-8 bytes, and whole-expression identity over name-sorted canonical
// values.

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// refCanonNum canonicalizes one JSON number token per the AGREED rules.
func refCanonNum(tok string) (string, error) {
	if len(tok) > 256 {
		return "", fmt.Errorf("number token %d bytes exceeds 256", len(tok))
	}
	s := tok
	neg := false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	mant, expPart := s, "0"
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mant, expPart = s[:i], s[i+1:]
	}
	intPart, fracPart := mant, ""
	if i := strings.IndexByte(mant, '.'); i >= 0 {
		intPart, fracPart = mant[:i], mant[i+1:]
	}
	for _, part := range []string{intPart, expPart} {
		if strings.TrimLeft(strings.TrimPrefix(strings.TrimPrefix(part, "+"), "-"), "0123456789") != "" || part == "" {
			return "", fmt.Errorf("malformed number token %q", tok)
		}
	}
	var exp int
	if _, err := fmt.Sscanf(expPart, "%d", &exp); err != nil {
		return "", fmt.Errorf("malformed exponent in %q", tok)
	}
	digits := intPart + fracPart
	if strings.Trim(digits, "0123456789") != "" || digits == "" {
		return "", fmt.Errorf("malformed digits in %q", tok)
	}
	k := exp - len(fracPart) // value = digits * 10^k
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0", nil // every zero spelling, negative included
	}
	trimmed := strings.TrimRight(digits, "0")
	k += len(digits) - len(trimmed)
	digits = trimmed
	sig := len(digits)
	mathExp := k + sig - 1
	if sig > 40 {
		return "", fmt.Errorf("%d significant digits exceeds 40", sig)
	}
	if mathExp > 200 || mathExp < -200 {
		return "", fmt.Errorf("mathematical exponent %d outside +-200", mathExp)
	}
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	switch {
	case k >= 0:
		b.WriteString(digits)
		b.WriteString(strings.Repeat("0", k))
	case -k < sig:
		b.WriteString(digits[:sig+k])
		b.WriteByte('.')
		b.WriteString(digits[sig+k:])
	default:
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", -k-sig))
		b.WriteString(digits)
	}
	return b.String(), nil
}

// refRank: false < true < numbers < strings.
func refRank(v any) int {
	switch x := v.(type) {
	case bool:
		if !x {
			return 0
		}
		return 1
	case json.Number:
		return 2
	case string:
		if raw, ok := strings.CutPrefix(x, "RAW:"); ok {
			_ = raw
			return 2 // RAW tokens are number spellings by corpus convention
		}
		return 3
	}
	return -1
}

func refCompare(a, b any) int {
	ra, rb := refRank(a), refRank(b)
	if ra != rb {
		return ra - rb
	}
	switch ra {
	case 2:
		x, y := new(big.Rat), new(big.Rat)
		if _, ok := x.SetString(numTok(a)); !ok {
			panic("unparseable number " + numTok(a))
		}
		if _, ok := y.SetString(numTok(b)); !ok {
			panic("unparseable number " + numTok(b))
		}
		return x.Cmp(y)
	case 3:
		return strings.Compare(a.(string), b.(string)) // byte order for UTF-8
	}
	return 0
}

func numTok(v any) string {
	switch x := v.(type) {
	case json.Number:
		return x.String()
	case string:
		return strings.TrimPrefix(x, "RAW:")
	}
	panic("not a number value")
}

// refCanonValue canonicalizes one scalar; RAW: strings are number tokens.
func refCanonValue(v any) (any, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case json.Number:
		c, err := refCanonNum(x.String())
		if err != nil {
			return nil, err
		}
		return json.Number(c), nil
	case string:
		if raw, ok := strings.CutPrefix(x, "RAW:"); ok {
			c, err := refCanonNum(raw)
			if err != nil {
				return nil, err
			}
			return json.Number(c), nil
		}
		if n := utf8.RuneCountInString(x); n > 512 {
			return nil, fmt.Errorf("%d decoded runes exceeds 512", n)
		}
		return x, nil
	}
	return nil, fmt.Errorf("unsupported scalar %T", v)
}

func refCanonIn(vals []any) ([]any, error) {
	if len(vals) == 0 {
		return nil, fmt.Errorf("empty in")
	}
	if len(vals) > 64 {
		return nil, fmt.Errorf("%d alternatives exceeds 64", len(vals))
	}
	out := make([]any, 0, len(vals))
	for _, v := range vals {
		switch v.(type) {
		case []any, map[string]any:
			return nil, fmt.Errorf("nested value inside in")
		case nil:
			return nil, fmt.Errorf("null inside in")
		}
		c, err := refCanonValue(v)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return refCompare(out[i], out[j]) < 0 })
	dedup := out[:0]
	for _, v := range out {
		if len(dedup) == 0 || refCompare(dedup[len(dedup)-1], v) != 0 {
			dedup = append(dedup, v)
		}
	}
	return dedup, nil
}

// refCanonExpr produces the reference canonical serialization used for
// identity comparison inside this test. It is NOT the engine's pinned
// byte encoding — identity cases compare reference-to-reference.
func refCanonExpr(m map[string]any) (string, error) {
	if len(m) == 0 || len(m) > 8 {
		return "", fmt.Errorf("%d names outside 1..8", len(m))
	}
	names := make([]string, 0, len(m))
	for n := range m {
		if r := utf8.RuneCountInString(n); r == 0 || r > 128 {
			return "", fmt.Errorf("name %q outside 1..128 runes", n)
		}
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q:", n)
		switch v := m[n].(type) {
		case nil:
			return "", fmt.Errorf("null value for %q", n)
		case map[string]any:
			if len(v) != 1 {
				return "", fmt.Errorf("operator object for %q must have exactly one member", n)
			}
			arr, ok := v["in"].([]any)
			if !ok {
				for op := range v {
					if op != "in" {
						return "", fmt.Errorf("unknown operator %q for %q", op, n)
					}
				}
				return "", fmt.Errorf("in for %q is not an array", n)
			}
			canon, err := refCanonIn(arr)
			if err != nil {
				return "", fmt.Errorf("%s: %w", n, err)
			}
			// Singleton collapse (Gepito's ruling, 2026-08-08): after
			// dedup, an IN of one IS direct equality — one predicate,
			// one identity. Raw cardinality was validated above.
			if len(canon) == 1 {
				writeScalar(&b, canon[0])
				continue
			}
			b.WriteString(`{"in":[`)
			for j, cv := range canon {
				if j > 0 {
					b.WriteByte(',')
				}
				writeScalar(&b, cv)
			}
			b.WriteString(`]}`)
		default:
			cv, err := refCanonValue(v)
			if err != nil {
				return "", fmt.Errorf("%s: %w", n, err)
			}
			writeScalar(&b, cv)
		}
	}
	b.WriteByte('}')
	return b.String(), nil
}

func writeScalar(b *strings.Builder, v any) {
	switch x := v.(type) {
	case bool:
		fmt.Fprintf(b, "%v", x)
	case json.Number:
		b.WriteString(x.String())
	case string:
		fmt.Fprintf(b, "%q", x)
	}
}

func loadCorpus(t *testing.T) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/filter_canonical_corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var c struct {
		Version int              `json:"version"`
		Cases   []map[string]any `json:"cases"`
	}
	if err := dec.Decode(&c); err != nil {
		t.Fatal(err)
	}
	if c.Version != 1 || len(c.Cases) == 0 {
		t.Fatalf("corpus version=%d cases=%d", c.Version, len(c.Cases))
	}
	return c.Cases
}

func TestFilterCanonicalCorpusSelfConsistent(t *testing.T) {
	kinds := map[string]int{}
	for i, c := range loadCorpus(t) {
		kind := c["kind"].(string)
		kinds[kind]++
		name := fmt.Sprintf("case %d (%s)", i, kind)
		switch kind {
		case "number":
			want := c["canonical"].(string)
			for _, sp := range c["spellings"].([]any) {
				tok := sp.(string) // spellings are literal number tokens
				got, err := refCanonNum(tok)
				if err != nil {
					t.Errorf("%s: %q: %v", name, tok, err)
					continue
				}
				if got != want {
					t.Errorf("%s: %q -> %q, want %q", name, tok, got, want)
				}
				// Value identity double-check via big.Rat.
				if refCompare(json.Number(tok), json.Number(want)) != 0 {
					t.Errorf("%s: %q does not equal its canonical %q by value", name, tok, want)
				}
			}
		case "in-set":
			got, err := refCanonIn(c["input"].([]any))
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			want := c["canonical"].([]any)
			if len(got) != len(want) {
				t.Errorf("%s: canonical length %d, want %d (%v)", name, len(got), len(want), got)
				continue
			}
			for j := range got {
				w, err := refCanonValue(want[j])
				if err != nil {
					t.Errorf("%s: bad expected value %v: %v", name, want[j], err)
					continue
				}
				if refCompare(got[j], w) != 0 || refRank(got[j]) != refRank(w) {
					t.Errorf("%s: position %d: got %v, want %v", name, j, got[j], w)
				}
			}
		case "identity":
			ca, errA := refCanonExpr(c["a"].(map[string]any))
			cb, errB := refCanonExpr(c["b"].(map[string]any))
			if errA != nil || errB != nil {
				t.Errorf("%s: %v / %v", name, errA, errB)
				continue
			}
			if equal := c["equal"].(bool); (ca == cb) != equal {
				t.Errorf("%s: equal=%v but\n a=%s\n b=%s", name, equal, ca, cb)
			}
		case "invalid":
			if _, err := refCanonExpr(c["filter"].(map[string]any)); err == nil {
				t.Errorf("%s: accepted invalid filter (%s)", name, c["reason"])
			}
		case "legacy-envelope":
			canon, err := refCanonExpr(c["filter"].(map[string]any))
			if err != nil {
				t.Errorf("%s: the legacy max envelope must stay valid: %v", name, err)
				continue
			}
			// Reference-encoding estimate of the canonical size; the
			// engine's pinned minimal encoding must also keep this
			// fixture under 32 KiB (raise the cap, never reject it).
			if n := len(canon); n > 32*1024 {
				t.Errorf("%s: reference canonical form is %d bytes (> 32 KiB)", name, n)
			}
		default:
			t.Errorf("%s: unknown kind", name)
		}
	}
	for _, k := range []string{"number", "in-set", "identity", "invalid", "legacy-envelope"} {
		if kinds[k] == 0 {
			t.Errorf("corpus has no %q cases", k)
		}
	}
}
