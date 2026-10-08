package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kurn-dev/kurn/engine/exact"
	"github.com/kurn-dev/kurn/engine/ngram"
)

func cacheChargeConfig(mode, fallback string) ListConfig {
	c := ListConfig{
		Analyzer:   AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:      MatchConfig{Mode: mode, Grams: []int{2, 3}, Fallback: fallback},
		Filterable: []FilterField{{Name: "program", Path: "program"}},
	}
	return c
}

func cacheChargeEntries() []Entry {
	return []Entry{
		{ID: "dup", Keys: []string{"discarded"}, Payload: json.RawMessage(`{"program":"old","pad":"discarded"}`)},
		{ID: "dup", Keys: []string{"example.com"}, Payload: json.RawMessage(`{"program":"SDN","pad":"123456789"}`)},
		{ID: "empty", Keys: []string{"zzzzzzzz"}, Payload: json.RawMessage{}},
		{ID: "nil", Keys: []string{"yyyyyyyy"}},
		{ID: "keyless", Payload: json.RawMessage(`{"program":"SDN","pad":"keyless-is-retained"}`)},
	}
}

func cacheChargePrepared(t *testing.T, l *List, q string) *FilteredQuery {
	t.Helper()
	p, err := l.PrepareFilteredQuery(q, QueryOpts{}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func cacheChargeOutcome(t *testing.T, p *FilteredQuery) (int64, []byte, string) {
	t.Helper()
	c, v, err := p.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return p.Cost(), b, v
}

func cacheChargeAssertSame(t *testing.T, label string, got, want *FilteredQuery) {
	t.Helper()
	gc, gb, gv := cacheChargeOutcome(t, got)
	wc, wb, wv := cacheChargeOutcome(t, want)
	if gc != wc || !bytes.Equal(gb, wb) {
		t.Fatalf("%s: cost/results differ: cost %d/%d results %s/%s", label, gc, wc, gb, wb)
	}
	if gv == "" || wv == "" {
		t.Fatalf("%s: empty version: %q/%q", label, gv, wv)
	}
}

func cacheChargeIndexes(t *testing.T, mode string, entries []Entry, short bool) (*ngram.Index, *exact.Index) {
	t.Helper()
	n := len(entries)
	if short {
		n--
	}
	if mode == "ngram" {
		b := ngram.NewBuilder(ngram.Config{Grams: []int{2, 3}})
		for i := 0; i < n; i++ {
			b.Add(uint32(i), entries[i].Keys)
		}
		return b.Finish(), nil
	}
	b := exact.NewBuilder()
	for i := 0; i < n; i++ {
		b.Add(uint32(i), entries[i].Keys)
	}
	idx, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return nil, idx
}

// Every prebuilt-index entry point must charge exactly like Replace. The
// fixture also pins last-wins dedupe, nil/empty payloads, retained keyless
// entries, shorter indexes, typed filters, exact fallback levels, repeated
// replacement, and snapshot isolation.
func TestCachePayloadChargeConstructors(t *testing.T) {
	for _, tc := range []struct{ name, mode, fallback, q string }{
		{"ngram", "ngram", "", "example.com"},
		{"exact-flat", "exact", "", "example.com"},
		{"exact-parent", "exact", "parent_domain", "sub.deep.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cacheChargeConfig(tc.mode, tc.fallback)
			entries := dedupeLastWins(cacheChargeEntries())
			rebuilt, _ := NewList("rebuilt", cfg)
			if err := rebuilt.Replace(cacheChargeEntries()); err != nil {
				t.Fatal(err)
			}
			want := cacheChargePrepared(t, rebuilt, tc.q)
			ng, ex := cacheChargeIndexes(t, tc.mode, entries, true)

			for _, info := range []bool{false, true} {
				got, _ := NewList("cached", cfg)
				var err error
				if tc.mode == "ngram" {
					if info {
						err = got.ReplaceWithIndexInfo(cacheChargeEntries(), ng, nil)
					} else {
						err = got.ReplaceWithIndex(cacheChargeEntries(), ng)
					}
				} else {
					if info {
						err = got.ReplaceWithExactIndexInfo(cacheChargeEntries(), ex, nil)
					} else {
						err = got.ReplaceWithExactIndex(cacheChargeEntries(), ex)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				p := cacheChargePrepared(t, got, tc.q)
				cacheChargeAssertSame(t, "constructor", p, want)
				plain, nilFilter := got.PrepareQuery(tc.q, QueryOpts{}), func() *FilteredQuery {
					p, e := got.PrepareFilteredQuery(tc.q, QueryOpts{}, nil)
					if e != nil {
						t.Fatal(e)
					}
					return p
				}()
				if nilFilter.Cost() != plain.Cost() {
					t.Fatalf("nil filter changed cost: %d != %d", nilFilter.Cost(), plain.Cost())
				}
				emptyFilter, err := got.PrepareFilteredQuery(tc.q, QueryOpts{}, map[string]string{})
				if err != nil || emptyFilter.Cost() != plain.Cost() {
					t.Fatalf("empty filter cost/error: %d %v, plain %d", emptyFilter.Cost(), err, plain.Cost())
				}
				typed, err := ParseTypedFilter([]byte(`{"program":"SDN"}`))
				if err != nil {
					t.Fatal(err)
				}
				tp, err := got.PrepareTypedFilteredQuery(tc.q, QueryOpts{}, typed)
				if err != nil {
					t.Fatal(err)
				}
				if tp.Cost() != p.Cost() {
					t.Fatalf("typed cost %d, legacy %d", tp.Cost(), p.Cost())
				}
				oldCost, oldAnswers, oldVersion := cacheChargeOutcome(t, p)
				for _, replacement := range [][]Entry{entries, entries[:2], append(entries, Entry{ID: "large", Keys: []string{"large.example.com"}, Payload: json.RawMessage(`{"program":"SDN","pad":"larger"}`)}), nil} {
					if tc.mode == "ngram" {
						err = got.ReplaceWithIndex(replacement, cacheChargeIndexesNgram(replacement))
					} else {
						err = got.ReplaceWithExactIndex(replacement, cacheChargeIndexesExact(t, replacement))
					}
					if err != nil {
						t.Fatal(err)
					}
					fresh, _ := NewList("fresh", cfg)
					if err := fresh.Replace(replacement); err != nil {
						t.Fatal(err)
					}
					cacheChargeAssertSame(t, "repeated replacement", cacheChargePrepared(t, got, tc.q), cacheChargePrepared(t, fresh, tc.q))
					c, a, v := cacheChargeOutcome(t, p)
					if c != oldCost || !bytes.Equal(a, oldAnswers) || v != oldVersion {
						t.Fatalf("captured snapshot changed: cost %d/%d answers %s/%s version %q/%q", c, oldCost, a, oldAnswers, v, oldVersion)
					}
				}

				// Reinstall content, then verify every rejected replacement leaves
				// the serving state, charge, answers and version untouched.
				if err := got.Replace(cacheChargeEntries()); err != nil {
					t.Fatal(err)
				}
				before := cacheChargePrepared(t, got, tc.q)
				beforeCost, beforeAnswers, beforeVersion := cacheChargeOutcome(t, before)
				badNG, badEX := cacheChargeIndexes(t, tc.mode, entries, false)
				var rejects []func() error
				if tc.mode == "ngram" {
					rejects = []func() error{
						func() error { return got.ReplaceWithIndex(entries, nil) },
						func() error { return got.ReplaceWithExactIndex(entries, cacheChargeIndexesExact(t, entries)) },
						func() error { return got.ReplaceWithIndex(entries[:1], badNG) },
						func() error { return got.ReplaceWithIndexInfo(entries, badNG, &IndexBuildInfo{Entries: 1}) },
					}
				} else {
					rejects = []func() error{
						func() error { return got.ReplaceWithExactIndex(entries, nil) },
						func() error { return got.ReplaceWithIndex(entries, cacheChargeIndexesNgram(entries)) },
						func() error { return got.ReplaceWithExactIndex(entries[:1], badEX) },
						func() error { return got.ReplaceWithExactIndexInfo(entries, badEX, &IndexBuildInfo{Entries: 1}) },
					}
				}
				for i, reject := range rejects {
					if err := reject(); err == nil {
						t.Fatalf("rejection %d accepted", i)
					}
					c, a, v := cacheChargeOutcome(t, cacheChargePrepared(t, got, tc.q))
					if c != beforeCost || !bytes.Equal(a, beforeAnswers) || v != beforeVersion {
						t.Fatalf("rejection %d mutated state", i)
					}
				}
			}

			// The independently derived retained payload total contributes once
			// per walked level, including keyless and excluding the old duplicate.
			zero := append([]Entry(nil), entries...)
			var retained int64
			for i := range zero {
				retained += int64(len(zero[i].Payload))
				zero[i].Payload = nil
			}
			zl, _ := NewList("zero", cfg)
			if err := zl.Replace(zero); err != nil {
				t.Fatal(err)
			}
			levels := int64(1)
			if tc.fallback == "parent_domain" {
				levels = 3
			}
			if delta := want.Cost() - cacheChargePrepared(t, zl, tc.q).Cost(); delta != retained*levels {
				t.Fatalf("payload term %d, want retained %d * levels %d", delta, retained, levels)
			}
		})
	}
}

func cacheChargeIndexesNgram(entries []Entry) *ngram.Index {
	b := ngram.NewBuilder(ngram.Config{Grams: []int{2, 3}})
	for i := range entries {
		b.Add(uint32(i), entries[i].Keys)
	}
	return b.Finish()
}

func cacheChargeIndexesExact(t *testing.T, entries []Entry) *exact.Index {
	b := exact.NewBuilder()
	for i := range entries {
		b.Add(uint32(i), entries[i].Keys)
	}
	x, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// A real Store restart must use the cache (base.idx is present) and remain
// equal to a forced rebuild, including journal replay, on two consecutive
// cache-backed opens.
func TestCachePayloadChargeStoreRestart(t *testing.T) {
	for _, mode := range []string{"ngram", "exact"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			st, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = st.CreateList("x", cacheChargeConfig(mode, "")); err != nil {
				t.Fatal(err)
			}
			if err = st.Replace("x", cacheChargeEntries()); err != nil {
				t.Fatal(err)
			}
			if err = st.Compact("x"); err != nil {
				t.Fatal(err)
			}
			if err = st.Upsert("x", []Entry{{ID: "journal", Keys: []string{"journal.example.com"}, Payload: json.RawMessage(`{"program":"SDN","pad":"journal"}`)}}); err != nil {
				t.Fatal(err)
			}
			live, _ := st.List("x")
			want := cacheChargePrepared(t, live, "example.com")
			wantCost, wantAnswers, wantVersion := cacheChargeOutcome(t, want)
			if err = st.Close(); err != nil {
				t.Fatal(err)
			}
			idx := filepath.Join(dir, "x", "base.idx")
			if fi, e := os.Stat(idx); e != nil || fi.Size() == 0 {
				t.Fatalf("cache path not provisioned: %v", e)
			}
			for restart := 1; restart <= 2; restart++ {
				re, e := Open(dir)
				if e != nil {
					t.Fatal(e)
				}
				l, ok := re.List("x")
				if !ok {
					t.Fatal("list missing")
				}
				gotCost, gotAnswers, gotVersion := cacheChargeOutcome(t, cacheChargePrepared(t, l, "example.com"))
				if gotCost != wantCost || !bytes.Equal(gotAnswers, wantAnswers) || gotVersion != wantVersion {
					t.Fatalf("restart %d cache mismatch", restart)
				}
				if err := re.Close(); err != nil {
					t.Fatal(err)
				}
			}

			// Copy the same owned fixture without base.idx: Open must rebuild,
			// providing an actual fallback oracle rather than a claimed one.
			rebuildDir := t.TempDir()
			if err := os.Mkdir(filepath.Join(rebuildDir, "x"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"config.json", "base.jsonl", "journal.jsonl"} {
				b, err := os.ReadFile(filepath.Join(dir, "x", name))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(rebuildDir, "x", name), b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			rebuilt, err := Open(rebuildDir)
			if err != nil {
				t.Fatal(err)
			}
			defer rebuilt.Close()
			rl, ok := rebuilt.List("x")
			if !ok {
				t.Fatal("rebuilt list missing")
			}
			rc, ra, rv := cacheChargeOutcome(t, cacheChargePrepared(t, rl, "example.com"))
			if rc != wantCost || !bytes.Equal(ra, wantAnswers) || rv != wantVersion {
				t.Fatalf("forced rebuild mismatch: cost %d/%d results %s/%s version %q/%q", rc, wantCost, ra, wantAnswers, rv, wantVersion)
			}
		})
	}
}
