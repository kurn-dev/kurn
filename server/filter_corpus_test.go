package server_test

// The shared C2 corpus through the real HTTP envelope. The independent
// reference runner proves the semantic fixtures; this runner proves the node
// accepts/rejects every same document and echoes the engine's canonical bytes.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

const filterCorpusSHA = "feebf8bdbc21135b8c43f38f7b8dbcba7d9cde4418b1713c2ef52890af6c8a7e"

type filterBoundaryCase struct {
	name  string
	raw   []byte
	valid bool
	field string
}

func loadFilterBoundaryCases(t *testing.T, path string) []filterBoundaryCase {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if got := hex.EncodeToString(digest[:]); got != filterCorpusSHA {
		t.Fatalf("shared corpus digest %s, want %s", got, filterCorpusSHA)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var corpus struct {
		Cases []map[string]any `json:"cases"`
	}
	if err := dec.Decode(&corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Cases) != 48 {
		t.Fatalf("shared corpus has %d cases, want 48", len(corpus.Cases))
	}
	var out []filterBoundaryCase
	add := func(name string, value any, valid bool) {
		m, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s filter is %T", name, value)
		}
		names := make([]string, 0, len(m))
		for field := range m {
			names = append(names, field)
		}
		sort.Strings(names)
		field := ""
		if len(names) > 0 {
			field = names[0]
		}
		out = append(out, filterBoundaryCase{name: name, raw: filterCorpusJSON(value), valid: valid, field: field})
	}
	for i, c := range corpus.Cases {
		kind := c["kind"].(string)
		base := fmt.Sprintf("case_%02d_%s", i, kind)
		switch kind {
		case "number":
			for j, spelling := range c["spellings"].([]any) {
				add(fmt.Sprintf("%s_%02d", base, j), map[string]any{"n": "RAW:" + spelling.(string)}, true)
			}
		case "in-set":
			add(base, map[string]any{"n": map[string]any{"in": c["input"]}}, true)
		case "identity":
			add(base+"_a", c["a"], true)
			add(base+"_b", c["b"], true)
		case "invalid":
			add(base, c["filter"], false)
		case "legacy-envelope":
			add(base, c["filter"], true)
		default:
			t.Fatalf("unknown corpus kind %q", kind)
		}
	}
	return out
}

// filterCorpusJSON retains RAW: number spellings while encoding all decoded
// corpus strings normally (including U+0000 and quote/backslash controls).
func filterCorpusJSON(v any) []byte {
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
			if token, ok := strings.CutPrefix(x, "RAW:"); ok {
				out = append(out, token...)
			} else {
				encoded, _ := json.Marshal(x)
				out = append(out, encoded...)
			}
		default:
			encoded, _ := json.Marshal(x)
			out = append(out, encoded...)
		}
	}
	appendValue(v)
	return out
}

func TestTypedFilterHTTPBoundaryCanonicalCorpus(t *testing.T) {
	ts, st := newFilterTS(t)
	if _, err := st.CreateList("invalid", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, Threshold: 0.5, TopK: 10},
	}); err != nil {
		t.Fatal(err)
	}
	for i, tc := range loadFilterBoundaryCases(t, "../engine/testdata/filter_canonical_corpus.json") {
		t.Run(tc.name, func(t *testing.T) {
			filter, parseErr := engine.ParseTypedFilter(tc.raw)
			if !tc.valid {
				if parseErr == nil {
					t.Fatalf("production parser accepted invalid corpus filter: %s", tc.raw)
				}
				resp, body := do(t, "POST", ts.URL+"/v1/query",
					`{"q":"x","lists":["invalid"],"filter":`+string(tc.raw)+`}`)
				var got struct {
					Error string `json:"error"`
				}
				_ = json.Unmarshal(body, &got)
				if resp.StatusCode != 400 || got.Error != parseErr.Error() || (tc.field != "" && !strings.Contains(got.Error, tc.field)) {
					t.Fatalf("invalid boundary: %d %s; parser=%v", resp.StatusCode, body, parseErr)
				}
				return
			}
			if parseErr != nil {
				t.Fatalf("production parser rejected valid corpus filter: %v: %s", parseErr, tc.raw)
			}
			list := fmt.Sprintf("corpus%03d", i)
			fields := make([]engine.FilterField, 0, len(filter.Names()))
			for _, name := range filter.Names() {
				fields = append(fields, engine.FilterField{Name: name, Path: "v"})
			}
			if _, err := st.CreateList(list, engine.ListConfig{
				Analyzer:   engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
				Match:      engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, Threshold: 0.5, TopK: 10},
				Filterable: fields,
			}); err != nil {
				t.Fatal(err)
			}
			resp, body := do(t, "POST", ts.URL+"/v1/query",
				`{"q":"x","lists":["`+list+`"],"filter":`+string(tc.raw)+`}`)
			var got struct {
				Filter json.RawMessage `json:"filter"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("response: %v: %s", err, body)
			}
			if resp.StatusCode != 200 || !bytes.Equal(got.Filter, filter.CanonicalJSON()) {
				t.Fatalf("canonical HTTP echo: status=%d got=%s want=%s", resp.StatusCode, got.Filter, filter.CanonicalJSON())
			}
		})
	}
}
