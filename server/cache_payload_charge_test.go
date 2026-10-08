package server_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/server"
)

// Cache-backed filtered admission has an exact boundary: the rebuilt
// prepared charge is accepted, while one byte less is refused. Running this
// test before the engine fix proves both modes really loaded base.idx: their
// undercharged reopened requests incorrectly pass at charge-minus-one.
func TestCachePayloadChargeAdmissionBoundary(t *testing.T) {
	for _, mode := range []string{"ngram", "exact"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			st, err := engine.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			cfg := engine.ListConfig{
				Analyzer:   engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
				Match:      engine.MatchConfig{Mode: mode, Grams: []int{2, 3}},
				Filterable: []engine.FilterField{{Name: "program", Path: "program"}},
			}
			l, err := st.CreateList("x", cfg)
			if err != nil {
				t.Fatal(err)
			}
			payload := fmt.Sprintf(`{"program":"SDN","pad":%q}`, strings.Repeat("p", 4096))
			if err := st.Replace("x", []engine.Entry{{ID: "x", Keys: []string{"example.com"}, Payload: []byte(payload)}}); err != nil {
				t.Fatal(err)
			}
			prepared, err := l.PrepareFilteredQuery("example.com", engine.QueryOpts{}, map[string]string{"program": "SDN"})
			if err != nil {
				t.Fatal(err)
			}
			budget := prepared.Cost()
			plainBudget := l.PrepareQuery("example.com", engine.QueryOpts{}).Cost()
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			st, err = engine.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })

			body := `{"q":"example.com","lists":["x"],"filter":{"program":"SDN"}}`
			for _, tc := range []struct {
				name   string
				budget int64
				status int
			}{
				{"exact", budget, 200},
				{"minus-one", budget - 1, 503},
			} {
				h := server.NewWith(st, server.Config{QueryMemBudget: tc.budget})
				req := httptest.NewRequest(http.MethodPost, "/v1/query", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, req)
				got, _ := io.ReadAll(rr.Result().Body)
				if rr.Code != tc.status {
					t.Fatalf("%s budget %d: status %d body %s, want %d", tc.name, tc.budget, rr.Code, got, tc.status)
				}
			}
			// The pre-existing no-filter charge and admission path stay unchanged.
			h := server.NewWith(st, server.Config{QueryMemBudget: plainBudget})
			req := httptest.NewRequest(http.MethodPost, "/v1/query", strings.NewReader(`{"q":"example.com","lists":["x"]}`))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != 200 {
				t.Fatalf("plain exact budget: %d %s", rr.Code, rr.Body.Bytes())
			}
		})
	}
}
