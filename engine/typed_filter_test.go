package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestTypedFilterCanonicalForm(t *testing.T) {
	f, err := engine.ParseTypedFilter([]byte(`{
		"z":{"in":["é","a",1.0,1e0,-0,true,false,false]},
		"a":1e-2
	}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":0.01,"z":{"in":[false,true,0,1,"a","é"]}}`
	if got := string(f.CanonicalJSON()); got != want {
		t.Fatalf("canonical form\n got: %s\nwant: %s", got, want)
	}
	if names := f.Names(); len(names) != 2 || names[0] != "a" || names[1] != "z" {
		t.Fatalf("canonical names: %v", names)
	}

	// Accessors must not let a caller mutate the prepared value.
	raw := f.CanonicalJSON()
	raw[1] = 'X'
	names := f.Names()
	names[0] = "X"
	if got := string(f.CanonicalJSON()); got != want {
		t.Fatalf("caller mutated immutable filter: %s", got)
	}
}

func TestTypedFilterSingletonInCollapsesToEquality(t *testing.T) {
	for _, raw := range []string{
		`{"p":{"in":["SDN"]}}`,
		`{"p":{"in":["SDN","SDN"]}}`,
	} {
		f, err := engine.ParseTypedFilter([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if got := string(f.CanonicalJSON()); got != `{"p":"SDN"}` {
			t.Fatalf("%s canonicalized to %s", raw, got)
		}
	}
}

func TestTypedFilterNumbers(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{`1`, `{"n":1}`},
		{`1.0`, `{"n":1}`},
		{`1e0`, `{"n":1}`},
		{`-0.000e+99`, `{"n":0}`},
		{`0.00100`, `{"n":0.001}`},
		{`1e-200`, `{"n":0.` + strings.Repeat("0", 199) + `1}`},
		{`1e200`, `{"n":1` + strings.Repeat("0", 200) + `}`},
		{`1234567890123456789012345678901234567890`, `{"n":1234567890123456789012345678901234567890}`},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			f, err := engine.ParseTypedFilter([]byte(`{"n":` + tc.raw + `}`))
			if err != nil {
				t.Fatal(err)
			}
			if got := string(f.CanonicalJSON()); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
	for _, raw := range []string{
		`1e201`,
		`1e-201`,
		`12345678901234567890123456789012345678901`,
	} {
		if _, err := engine.ParseTypedFilter([]byte(`{"n":` + raw + `}`)); err == nil {
			t.Fatalf("accepted over-limit number %s", raw)
		}
	}
}

func TestTypedFilterRejectsInvalidGrammarDeterministically(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{`null`, `filter must be a JSON object`},
		{`{"z":null,"a":null}`, `filter value for "a" must not be null`},
		{`{"a":{"in":[]}}`, `empty in set`},
		{`{"a":{"wat":[1]}}`, `unknown operator "wat"`},
		{`{"a":{"in":[1],"wat":[2]}}`, `exactly one operator member`},
		{`{"a":{"in":[null]}}`, `nested or null`},
		{`{"a":{"in":[{}]}}`, `nested or null`},
		{`{"a":[1]}`, `must be a string, boolean, number`},
		{`{"a":null}`, `must not be null`},
		{`{"a":1,"\u0061":2}`, `duplicate filter name "a"`},
		{`{"a":{"in":[1],"\u0069n":[2]}}`, `duplicate operator "in"`},
	}
	for _, tc := range tests {
		if _, err := engine.ParseTypedFilter([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: got %v, want substring %q", tc.raw, err, tc.want)
		}
	}

	var alts []string
	for i := 0; i < 65; i++ {
		alts = append(alts, "1")
	}
	if _, err := engine.ParseTypedFilter([]byte(`{"a":{"in":[` + strings.Join(alts, ",") + `]}}`)); err == nil || !strings.Contains(err.Error(), "more than 64") {
		t.Fatalf("65 raw alternatives: %v", err)
	}
}

func TestTypedFilterLegacyMaximumEnvelopeRemainsValid(t *testing.T) {
	values := make(map[string]string, 8)
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("%d%s", i, strings.Repeat("界", 127))
		values[name] = strings.Repeat("🜁", 512)
	}
	f, err := engine.StringTypedFilter(values)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(f.CanonicalJSON()); got > 32<<10 {
		t.Fatalf("max legacy envelope canonicalized to %d bytes", got)
	}
	if !json.Valid(f.CanonicalJSON()) {
		t.Fatal("max legacy canonical form is not valid JSON")
	}
}

func typedList(t *testing.T) *engine.List {
	t.Helper()
	cfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, Threshold: 0.5, TopK: 20},
		Filterable: []engine.FilterField{
			{Name: "b", Path: "b"},
			{Name: "n", Path: "n"},
			{Name: "v", Path: "v"},
		},
	}
	l, err := engine.NewList("typed", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{
		{ID: "e1", Keys: []string{"dana kovak"}, Payload: json.RawMessage(`{"b":true,"n":1.00,"v":"1"}`)},
		{ID: "e2", Keys: []string{"dana kovak"}, Payload: json.RawMessage(`{"b":false,"n":2,"v":false}`)},
	}); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestTypedFilterEvaluationIsTypeExact(t *testing.T) {
	l := typedList(t)
	tests := []struct {
		filter string
		ids    []string
	}{
		{`{"n":1e0}`, []string{"e1"}},
		{`{"n":"1"}`, nil},
		{`{"v":"1"}`, []string{"e1"}},
		{`{"v":1}`, nil},
		{`{"v":{"in":[false,1,"no"]}}`, []string{"e2"}},
		{`{"b":{"in":[true,2]}}`, []string{"e1"}},
	}
	for _, tc := range tests {
		t.Run(tc.filter, func(t *testing.T) {
			f, err := engine.ParseTypedFilter([]byte(tc.filter))
			if err != nil {
				t.Fatal(err)
			}
			got, _, err := l.QueryTypedFilteredCtx(context.Background(), "dana kovak", engine.QueryOpts{}, f)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.ids) {
				t.Fatalf("got %+v, want %v", got, tc.ids)
			}
			for i := range got {
				if got[i].EntryID != tc.ids[i] {
					t.Fatalf("got %+v, want %v", got, tc.ids)
				}
			}
		})
	}
}

func TestTypedFilterPreparedValueAndLegacyWrapper(t *testing.T) {
	l := typedList(t)
	f, err := engine.ParseTypedFilter([]byte(`{"b":{"in":[true,false,true]}}`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := l.PrepareTypedFilteredQuery("dana kovak", engine.QueryOpts{}, f)
	if err != nil {
		t.Fatal(err)
	}
	if p.AppliedFilter() != nil {
		t.Fatal("typed query must not manufacture a legacy string map")
	}
	if !bytes.Equal(p.AppliedFilterJSON(), []byte(`{"b":{"in":[false,true]}}`)) {
		t.Fatalf("typed echo: %s", p.AppliedFilterJSON())
	}

	legacy, err := l.PrepareFilteredQuery("dana kovak", engine.QueryOpts{}, map[string]string{"v": "1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := legacy.AppliedFilter(); len(got) != 1 || got["v"] != "1" {
		t.Fatalf("legacy applied map: %v", got)
	}
	if got := string(legacy.AppliedFilterJSON()); got != `{"v":"1"}` {
		t.Fatalf("legacy canonical echo: %s", got)
	}
}

// TestTypedFilterAgainstCanonicalCorpus is the heavy implementation's Go
// execution of Claudio's independent corpus. The reference self-test lives in
// filter_canon_corpus_test.go; this test feeds the same semantic cases through
// the production parser/canonicalizer.
func TestTypedFilterAgainstCanonicalCorpus(t *testing.T) {
	for i, c := range loadCorpus(t) {
		kind := c["kind"].(string)
		name := fmt.Sprintf("case_%02d_%s", i, kind)
		t.Run(name, func(t *testing.T) {
			switch kind {
			case "number":
				want := `{"n":` + c["canonical"].(string) + `}`
				for _, spelling := range c["spellings"].([]any) {
					f, err := engine.ParseTypedFilter([]byte(`{"n":` + spelling.(string) + `}`))
					if err != nil {
						t.Fatalf("%s: %v", spelling, err)
					}
					if got := string(f.CanonicalJSON()); got != want {
						t.Fatalf("%s -> %s, want %s", spelling, got, want)
					}
				}
			case "in-set":
				input := map[string]any{"n": map[string]any{"in": c["input"]}}
				f, err := engine.ParseTypedFilter(corpusJSON(input))
				if err != nil {
					t.Fatal(err)
				}
				canon, err := refCanonIn(c["input"].([]any))
				if err != nil {
					t.Fatal(err)
				}
				var expected any = map[string]any{"in": canon}
				if len(canon) == 1 { // singleton-IN identity ruling
					expected = canon[0]
				}
				if want := string(corpusJSON(map[string]any{"n": expected})); string(f.CanonicalJSON()) != want {
					t.Fatalf("got %s, want %s", f.CanonicalJSON(), want)
				}
			case "invalid":
				if _, err := engine.ParseTypedFilter(corpusJSON(c["filter"])); err == nil {
					t.Fatalf("accepted invalid filter: %s", c["reason"])
				}
			case "identity":
				a, errA := engine.ParseTypedFilter(corpusJSON(c["a"]))
				b, errB := engine.ParseTypedFilter(corpusJSON(c["b"]))
				if errA != nil || errB != nil {
					t.Fatalf("identity parse: %v / %v", errA, errB)
				}
				equal := bytes.Equal(a.CanonicalJSON(), b.CanonicalJSON())
				if equal != c["equal"].(bool) {
					t.Fatalf("equal=%v, got\n a=%s\n b=%s", c["equal"], a.CanonicalJSON(), b.CanonicalJSON())
				}
			case "legacy-envelope":
				f, err := engine.ParseTypedFilter(corpusJSON(c["filter"]))
				if err != nil {
					t.Fatal(err)
				}
				if len(f.CanonicalJSON()) > 32<<10 {
					t.Fatalf("legacy envelope is %d bytes", len(f.CanonicalJSON()))
				}
			}
		})
	}
}

func corpusJSON(v any) []byte {
	var out []byte
	var appendValue func(any)
	appendValue = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			names := make([]string, 0, len(x))
			for name := range x {
				names = append(names, name)
			}
			sort.Strings(names)
			out = append(out, '{')
			for i, name := range names {
				if i > 0 {
					out = append(out, ',')
				}
				key, _ := json.Marshal(name)
				out = append(out, key...)
				out = append(out, ':')
				appendValue(x[name])
			}
			out = append(out, '}')
		case []any:
			out = append(out, '[')
			for i, value := range x {
				if i > 0 {
					out = append(out, ',')
				}
				appendValue(value)
			}
			out = append(out, ']')
		case string:
			if raw, ok := strings.CutPrefix(x, "RAW:"); ok {
				out = append(out, raw...)
			} else {
				value, _ := json.Marshal(x)
				out = append(out, value...)
			}
		default:
			value, _ := json.Marshal(x)
			out = append(out, value...)
		}
	}
	appendValue(v)
	return out
}
