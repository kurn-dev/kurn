package engine

// Scanner tests: table cases for the traversal/equality contract, and a
// differential fuzz against an encoding/json reference walker — the two
// implementations must agree on every input, with errors only for
// malformed JSON.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"
)

func segsClean(segs []string) bool {
	for _, s := range segs {
		if !cleanASCII(s) {
			return false
		}
	}
	return true
}

func TestPathMatchTable(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		path    []string
		want    string
		match   bool
		wantErr bool
	}{
		{"top-level hit", `{"program":"SDN"}`, []string{"program"}, "SDN", true, false},
		{"top-level miss", `{"program":"OFAC"}`, []string{"program"}, "SDN", false, false},
		{"nested hit", `{"meta":{"program":"SDN"}}`, []string{"meta", "program"}, "SDN", true, false},
		{"nested miss", `{"meta":{"program":"x"}}`, []string{"meta", "program"}, "SDN", false, false},
		{"missing path", `{"other":"SDN"}`, []string{"program"}, "SDN", false, false},
		{"empty object", `{}`, []string{"program"}, "SDN", false, false},
		{"array descent", `{"progs":[{"program":"x"},{"program":"SDN"}]}`, []string{"progs", "program"}, "SDN", true, false},
		{"final string array", `{"programs":["x","SDN"]}`, []string{"programs"}, "SDN", true, false},
		{"nested string array", `{"programs":[["x"],["SDN"]]}`, []string{"programs"}, "SDN", true, false},
		{"escaped value matches", `{"program":"SD\u004e"}`, []string{"program"}, "SDN", true, false},
		{"escaped key matches", `{"pro\u0067ram":"SDN"}`, []string{"program"}, "SDN", true, false},
		{"unicode value", `{"program":"SÄKN"}`, []string{"program"}, "SÄKN", true, false},
		{"escaped unicode want", `{"program":"SÄKN"}`, []string{"program"}, "SÄKN", true, false},
		{"number never matches", `{"program":42}`, []string{"program"}, "42", false, false},
		{"bool never matches", `{"program":true}`, []string{"program"}, "true", false, false},
		{"null never matches", `{"program":null}`, []string{"program"}, "", false, false},
		{"object never matches", `{"program":{"x":"SDN"}}`, []string{"program"}, "SDN", false, false},
		{"root string non-match", `"SDN"`, []string{"program"}, "SDN", false, false},
		{"root number non-match", `42`, []string{"program"}, "SDN", false, false},
		{"root array of objects", `[{"program":"SDN"}]`, []string{"program"}, "SDN", true, false},
		{"whitespace tolerant", "{  \"program\" :\t\"SDN\" }", []string{"program"}, "SDN", true, false},
		{"duplicate keys any-match", `{"program":"x","program":"SDN"}`, []string{"program"}, "SDN", true, false},
		{"truncated", `{"program":"SDN"`, []string{"program"}, "SDN", false, true},
		{"unterminated string", `{"program":"SDN}`, []string{"program"}, "SDN", false, true},
		{"mismatched bracket", `{"program":["SDN"}`, []string{"program"}, "SDN", false, true},
		{"bad separator", `{"a":1 "program":"SDN"}`, []string{"program"}, "SDN", false, true},
		{"garbage", `not json at all`, []string{"program"}, "SDN", false, true},
		{"empty payload", ``, []string{"program"}, "SDN", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pathMatch([]byte(tc.payload), tc.path, tc.want, cleanASCII(tc.want), segsClean(tc.path))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got match=%v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.match {
				t.Fatalf("match=%v, want %v", got, tc.match)
			}
		})
	}
}

// refMatch is the decode-based reference: dot-path walk with array
// auto-descent; a reached JSON string equals when decoded-equal; other
// scalars never match. Malformed input errors. json.Number avoids
// float-overflow false errors on huge literals; the second Decode pins
// trailing-data strictness. Note: duplicate object keys collapse
// last-wins here while the scanner matches any occurrence (the
// conservative screening reading); the generated corpus has no
// duplicate keys, so the two agree.
func refMatch(payload []byte, segs []string, want string) (bool, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return false, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return false, fmt.Errorf("trailing data")
	}
	return refWalk(v, segs, want), nil
}

func refWalk(v any, segs []string, want string) bool {
	switch n := v.(type) {
	case map[string]any:
		c, ok := n[segs[0]]
		if !ok {
			return false
		}
		if len(segs) == 1 {
			return refScalar(c, want)
		}
		return refWalk(c, segs[1:], want)
	case []any:
		for _, e := range n {
			if refWalk(e, segs, want) {
				return true
			}
		}
	}
	return false
}

func refScalar(v any, want string) bool {
	switch n := v.(type) {
	case string:
		return n == want
	case []any:
		for _, e := range n {
			if refScalar(e, want) {
				return true
			}
		}
	}
	return false
}

// randomDoc builds nested JSON with occasional escapes and unicode.
func randomDoc(r *rand.Rand, depth int) any {
	strs := []string{"SDN", "OFAC", "x", "SÄKN", "with space", "", "esc\"aped", "back\\slash"}
	switch {
	case depth > 3:
		return strs[r.Intn(len(strs))]
	default:
		switch r.Intn(4) {
		case 0:
			m := map[string]any{}
			for i := 0; i < r.Intn(4); i++ {
				m[fmt.Sprintf("k%d", r.Intn(4))] = randomDoc(r, depth+1)
			}
			if r.Intn(3) == 0 {
				m["program"] = strs[r.Intn(len(strs))]
			}
			return m
		case 1:
			var a []any
			for i := 0; i < r.Intn(3); i++ {
				a = append(a, randomDoc(r, depth+1))
			}
			return a
		case 2:
			return strs[r.Intn(len(strs))]
		default:
			return r.Intn(100)
		}
	}
}

func TestPathMatchDifferential(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	paths := [][]string{{"program"}, {"meta", "program"}, {"k0"}, {"k1", "k2"}, {"progs", "program"}}
	wants := []string{"SDN", "OFAC", "x", "SÄKN", ""}
	for i := 0; i < 20000; i++ {
		doc := randomDoc(r, 0)
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		segs := paths[r.Intn(len(paths))]
		want := wants[r.Intn(len(wants))]
		got, gerr := pathMatch(raw, segs, want, cleanASCII(want), segsClean(segs))
		wref, werr := refMatch(raw, segs, want)
		if (gerr != nil) != (werr != nil) {
			t.Fatalf("error mismatch on %s path %v: scanner %v, ref %v", raw, segs, gerr, werr)
		}
		if gerr == nil && got != wref {
			t.Fatalf("match mismatch on %s path %v want %q: scanner %v, ref %v", raw, segs, want, got, wref)
		}
	}
}

// Multi-term differential: the one-pass trie evaluator must agree with
// per-term decode-reference ANDing on every input.
func TestCompiledFilterMultiTermDifferential(t *testing.T) {
	cfg := ListConfig{
		Analyzer: AnalyzerConfig{Steps: []string{"lowercase"}},
		Match:    MatchConfig{Mode: "ngram"},
		Filterable: []FilterField{
			{Name: "program", Path: "program"},
			{Name: "nested", Path: "meta.program"},
			{Name: "k0", Path: "k0"},
			{Name: "listed", Path: "progs.program"},
		},
	}
	l, err := NewList("multi", cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(7))
	names := []string{"program", "nested", "k0", "listed"}
	paths := map[string][]string{"program": {"program"}, "nested": {"meta", "program"}, "k0": {"k0"}, "listed": {"progs", "program"}}
	wants := []string{"SDN", "OFAC", "x", "SÄKN", ""}
	for i := 0; i < 20000; i++ {
		doc := randomDoc(r, 0)
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		// 1-3 random terms (shuffled name subset)
		perm := r.Perm(len(names))
		n := 1 + r.Intn(3)
		f := map[string]string{}
		var picked []string
		for _, idx := range perm[:n] {
			f[names[idx]] = wants[r.Intn(len(wants))]
			picked = append(picked, names[idx])
		}
		cf, err := l.compileFilter(f)
		if err != nil {
			t.Fatal(err)
		}
		got, gerr := cf.eval(raw)
		want := true
		var werr error
		for _, name := range picked {
			m, err := refMatch(raw, paths[name], f[name])
			if err != nil {
				werr = err
				break
			}
			if !m {
				want = false
			}
		}
		if (gerr != nil) != (werr != nil) {
			t.Fatalf("error mismatch on %s filter %v: eval %v, ref %v", raw, f, gerr, werr)
		}
		if gerr == nil && got != want {
			t.Fatalf("eval mismatch on %s filter %v: got %v want %v", raw, f, got, want)
		}
	}
}

// Deterministic errors: with two independently invalid names, the same
// offender must be named on every call.
func TestCompileFilterDeterministicError(t *testing.T) {
	cfg := ListConfig{
		Analyzer:   AnalyzerConfig{Steps: []string{"lowercase"}},
		Match:      MatchConfig{Mode: "ngram"},
		Filterable: []FilterField{{Name: "program", Path: "program"}},
	}
	l, err := NewList("det", cfg)
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("v", maxFilterValueRunes+1)
	first := ""
	for i := 0; i < 100; i++ {
		_, err := l.compileFilter(map[string]string{"aaa": big, "bbb": big})
		if err == nil {
			t.Fatal("invalid filter accepted")
		}
		if first == "" {
			first = err.Error()
		} else if err.Error() != first {
			t.Fatalf("nondeterministic error: %q then %q", first, err)
		}
	}
	if !strings.Contains(first, `"aaa"`) {
		t.Fatalf("sorted-first offender not named: %s", first)
	}
}

func FuzzPathMatch(f *testing.F) {
	f.Add(`{"program":"SDN"}`, "program", "SDN")
	f.Add(`{"meta":{"program":"SD\u004e"}}`, "meta.program", "SDN")
	f.Add(`[{"programs":["x","SDN"]}]`, "programs", "SDN")
	f.Add(`{"program":"SDN"`, "program", "SDN")
	f.Add(`broken`, "program", "SDN")
	f.Fuzz(func(t *testing.T, payload, path, want string) {
		segs := strings.Split(path, ".")
		got, gerr := pathMatch([]byte(payload), segs, want, cleanASCII(want), segsClean(segs))
		wref, werr := refMatch([]byte(payload), segs, want)
		if (gerr != nil) != (werr != nil) {
			t.Fatalf("error mismatch on %q: scanner %v, ref %v", payload, gerr, werr)
		}
		if gerr == nil && got != wref {
			t.Fatalf("match mismatch on %q path %q want %q: scanner %v, ref %v", payload, path, want, got, wref)
		}
	})
}
