package builder_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/prepared"
	"github.com/kurn-dev/kurn/engine/prepared/builder"
)

func ngramConfig() engine.ListConfig {
	return engine.ListConfig{Analyzer: engine.AnalyzerConfig{Preset: "person-name"}, Match: engine.MatchConfig{Mode: "ngram"}}
}

func exactConfig() engine.ListConfig {
	return engine.ListConfig{Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}}, Match: engine.MatchConfig{Mode: "exact"}}
}

func filteredConfig() engine.ListConfig {
	c := ngramConfig()
	c.Match.Threshold = 0.5
	c.Filterable = []engine.FilterField{{Name: "program", Path: "program"}, {Name: "country", Path: "addr.country"}}
	return c
}

// testEntries produces entries whose IDs, keys and payloads carry every
// character class encoding/json escapes or passes through: <, >, &, U+2028,
// U+2029, quotes, backslashes, control bytes and non-ASCII.
func testEntries(n int, seed int64) []engine.Entry {
	rng := rand.New(rand.NewSource(seed))
	first := []string{"Anna", "José", "Ólafur", "Zoë", "Li", "O'Brien", "Smith & Co", "<Acme>", "Quote\"d", "Line\u2028Sep", "Para\u2029Sep", "Back\\slash", "Ctl\x01"}
	last := []string{"Smith", "Müller", "Holdings Ltd", "Petrov", "Nakamura", "de la Cruz", "Øster", "Şahin"}
	programs := []string{"SDGT", "SDN", "IRAN", "UKRAINE-EO13662"}
	countries := []string{"US", "DE", "IR", "RU"}
	out := make([]engine.Entry, n)
	for i := range out {
		f, l := first[rng.Intn(len(first))], last[rng.Intn(len(last))]
		e := engine.Entry{ID: fmt.Sprintf("E%05d<&>\u2028", i*31%99991), Keys: []string{f + " " + l}}
		if i%3 == 0 {
			e.Keys = append(e.Keys, l+", "+f, strings.ToUpper(f))
		}
		switch i % 4 {
		case 0: // no payload
		case 1: // non-canonical spacing and raw characters json escapes
			e.Payload = json.RawMessage(fmt.Sprintf(`{ "program" : %q, "note": "<b>&amp;`+"\u2028\u2029"+` \"x\"" }`, programs[i%len(programs)]))
		default:
			p, _ := json.Marshal(map[string]any{"program": programs[rng.Intn(len(programs))], "addr": map[string]string{"country": countries[rng.Intn(len(countries))]}, "i": i})
			e.Payload = p
		}
		out[i] = e
	}
	// Keyless shapes: an empty key list and a null one.
	out[1].Keys = []string{}
	out[2].Keys = nil
	return out
}

func encode(t *testing.T, name string, cfg engine.ListConfig, entries []engine.Entry) ([]byte, *prepared.View, *prepared.Verified) {
	t.Helper()
	buf, ver, err := builder.Encode(name, cfg, entries, builder.Options{Source: "unit-test"})
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := prepared.VerifyBytes(buf, prepared.LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	return buf, v, ver
}

// viewEntries rebuilds the entry slice from a view, borrowing its bytes the
// way the engine's prepared loader will.
func viewEntries(v *prepared.View) []engine.Entry {
	out := make([]engine.Entry, v.Len())
	for i := range out {
		out[i] = engine.Entry{ID: v.ID(i), Keys: v.Keys(i), Payload: v.Payload(i)}
	}
	return out
}

func canon(t *testing.T, p []byte) []byte {
	c, err := prepared.CanonicalPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// loadedList builds an engine list over the artifact's own index and
// records, as a prepared loader would.
func loadedList(t *testing.T, name string, v *prepared.View) *engine.List {
	cfg, err := builder.ListConfig(v)
	if err != nil {
		t.Fatal(err)
	}
	l, err := engine.NewList(name, cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := v.Build()
	bi := &engine.IndexBuildInfo{Entries: b.Entries, DroppedKeys: b.DroppedKeys, KeylessEntries: b.KeylessEntries}
	if cfg.Match.Mode == "ngram" {
		idx, err := v.NgramIndex()
		if err != nil {
			t.Fatal(err)
		}
		err = l.ReplaceWithIndexInfo(viewEntries(v), idx, bi)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		idx, err := v.ExactIndex()
		if err != nil {
			t.Fatal(err)
		}
		if err := l.ReplaceWithExactIndexInfo(viewEntries(v), idx, bi); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func queries(entries []engine.Entry) []string {
	qs := []string{"anna smith", "muller", "holdings", "acme", "zzz nothing", "jose de la cruz", "o'brien petrov"}
	for i := 0; i < len(entries); i += 17 {
		qs = append(qs, entries[i].Keys...)
	}
	return qs
}

func sameCandidates(t *testing.T, what string, got, want []engine.Candidate) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d candidates, want %d", what, len(got), len(want))
	}
	for i := range got {
		g, w := got[i], want[i]
		if g.EntryID != w.EntryID || g.Score != w.Score || g.Key != w.Key || !bytes.Equal(g.Payload, canon(t, w.Payload)) {
			t.Fatalf("%s: candidate %d = %+v, want %+v", what, i, g, w)
		}
	}
}

func TestRoundTripModes(t *testing.T) {
	for _, tc := range []struct {
		mode string
		cfg  engine.ListConfig
	}{{"ngram", ngramConfig()}, {"exact", exactConfig()}, {"filtered", filteredConfig()}} {
		t.Run(tc.mode, func(t *testing.T) {
			entries := testEntries(600, 11)
			if tc.mode == "filtered" {
				// The engine's filters refuse a candidate with no payload
				// (a malformed-payload error, never a silent non-match), so
				// give every entry one here to compare actual results.
				for i := range entries {
					if len(entries[i].Payload) == 0 {
						entries[i].Payload = json.RawMessage(`{"program":"NONE","addr":{"country":"XX"}}`)
					}
				}
			}
			orig := slices.Clone(entries)
			_, v, ver := encode(t, "list-"+tc.mode, tc.cfg, entries)
			if !slices.EqualFunc(entries, orig, func(a, b engine.Entry) bool {
				return a.ID == b.ID && bytes.Equal(a.Payload, b.Payload) && slices.Equal(a.Keys, b.Keys)
			}) {
				t.Fatal("builder modified the caller's entries")
			}
			// Records identical to the input (payloads in canonical form).
			if v.Len() != len(entries) || ver.Entries != len(entries) {
				t.Fatalf("%d records, want %d", v.Len(), len(entries))
			}
			for i, e := range entries {
				keys := v.Keys(i)
				if v.ID(i) != e.ID || !slices.Equal(keys, e.Keys) || (keys == nil) != (e.Keys == nil) || !bytes.Equal(v.Payload(i), canon(t, e.Payload)) {
					t.Fatalf("record %d differs: %q %q %q", i, v.ID(i), keys, v.Payload(i))
				}
			}
			// The index equals the one the engine builds from the same
			// entries.
			direct, err := engine.NewList("direct", tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := direct.Replace(slices.Clone(entries)); err != nil {
				t.Fatal(err)
			}
			if ng := direct.BaseNgram(); ng != nil {
				post := ng.Postings()
				if v.GramCount() != len(post) {
					t.Fatalf("%d grams, engine built %d", v.GramCount(), len(post))
				}
				for i := 0; i < v.GramCount(); i++ {
					bm, err := v.Postings(i)
					if err != nil {
						t.Fatal(err)
					}
					want := post[v.Gram(i)]
					if want == nil || !bm.Equals(want) || bm.GetCardinality() != want.GetCardinality() {
						t.Fatalf("gram %q differs from the engine's", v.Gram(i))
					}
				}
				if v.NumOrds() != ng.NumOrds() {
					t.Fatalf("numOrds %d, engine %d", v.NumOrds(), ng.NumOrds())
				}
			} else {
				ex := direct.BaseExact()
				restored, err := v.ExactIndex()
				if err != nil {
					t.Fatal(err)
				}
				if restored.Keys() != ex.Keys() || restored.NumOrds() != ex.NumOrds() {
					t.Fatalf("restored %d keys / %d ords, engine %d / %d", restored.Keys(), restored.NumOrds(), ex.Keys(), ex.NumOrds())
				}
				for _, k := range ex.SortedKeys() {
					if !slices.Equal(restored.Lookup(k), ex.Lookup(k)) {
						t.Fatalf("exact key %q: %v, engine %v", k, restored.Lookup(k), ex.Lookup(k))
					}
				}
			}
			// Differential results: a list served from the artifact answers
			// every query exactly like the directly built one.
			loaded := loadedList(t, "list-"+tc.mode, v)
			if a, b := direct.Status(), loaded.Status(); a.Entries != b.Entries || a.DroppedKeys != b.DroppedKeys || a.KeylessEntries != b.KeylessEntries || b.UnindexedEntries != 0 {
				t.Fatalf("status %+v, direct %+v", b, a)
			}
			var compared, comparedFiltered int
			for _, q := range queries(entries) {
				opts := engine.QueryOpts{Threshold: 0.3, TopK: 25}
				got := loaded.Query(q, opts)
				sameCandidates(t, "query "+q, got, direct.Query(q, opts))
				compared += len(got)
				if tc.mode == "filtered" {
					for _, f := range []map[string]string{{"program": "SDGT"}, {"country": "DE"}, {"program": "SDN", "country": "US"}} {
						fa, err := direct.PrepareFilteredQuery(q, opts, f)
						if err != nil {
							t.Fatal(err)
						}
						fb, err := loaded.PrepareFilteredQuery(q, opts, f)
						if err != nil {
							t.Fatal(err)
						}
						ca, _, errA := fa.Execute(context.Background())
						cb, _, errB := fb.Execute(context.Background())
						if fmt.Sprint(errA) != fmt.Sprint(errB) || errA != nil {
							t.Fatalf("filtered %q %v: errors %v vs direct %v", q, f, errB, errA)
						}
						sameCandidates(t, fmt.Sprintf("filtered %q %v", q, f), cb, ca)
						comparedFiltered += len(cb)
					}
				}
			}
			if compared == 0 || (tc.mode == "filtered") != (comparedFiltered > 0) {
				t.Fatalf("vacuous comparison: %d candidates, %d filtered", compared, comparedFiltered)
			}
			t.Logf("compared %d candidates (%d filtered)", compared, comparedFiltered)
		})
	}
}

// TestIdentityMatchesEngineStore: the artifact's logical base ID is what
// the engine store computes and writes for the same entries — the base
// identity it reports, the SHA-256 of its base.jsonl, and the version stamp
// it serves.
func TestIdentityMatchesEngineStore(t *testing.T) {
	for name, cfg := range map[string]engine.ListConfig{"ngram": ngramConfig(), "exact": exactConfig(), "filtered": filteredConfig()} {
		entries := testEntries(250, 3)
		_, v, ver := encode(t, name, cfg, entries)
		dir := t.TempDir()
		st, err := engine.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateList(name, cfg); err != nil {
			t.Fatal(err)
		}
		if err := st.Replace(name, slices.Clone(entries)); err != nil {
			t.Fatal(err)
		}
		l, _ := st.List(name)
		raw, err := os.ReadFile(filepath.Join(dir, name, "base.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if ver.BaseID != hex.EncodeToString(sum[:]) || ver.BaseID != l.BaseIDForArtifact() {
			t.Fatalf("%s: artifact base %s, base.jsonl %x, store %s", name, ver.BaseID, sum, l.BaseIDForArtifact())
		}
		if ver.ConfigDigest != l.ConfigDigest() {
			t.Fatalf("%s: config digest %s, store %s", name, ver.ConfigDigest, l.ConfigDigest())
		}
		if stamp := fmt.Sprintf("%s@%d+j0+c%s", ver.BaseID, ver.Entries, ver.ConfigDigest); stamp != l.Version() {
			t.Fatalf("%s: artifact implies stamp %s, store serves %s", name, stamp, l.Version())
		}
		// Every stored payload is byte-identical to the store's base line
		// value, i.e. what a restarted store would serve.
		lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
		for i, line := range lines {
			var e engine.Entry
			if err := json.Unmarshal(line, &e); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(e.Payload, v.Payload(i)) || e.ID != v.ID(i) {
				t.Fatalf("%s: record %d payload %q, base line has %q", name, i, v.Payload(i), e.Payload)
			}
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		if s := v.Build().Keys; s != l.KeyCount() {
			t.Fatalf("%s: %d keys, store counts %d", name, s, l.KeyCount())
		}
	}
}

func TestDeterministicRebuildFromRecords(t *testing.T) {
	// Index agreement, the differential way: re-encoding the artifact's own
	// records reproduces it byte for byte.
	for name, cfg := range map[string]engine.ListConfig{"ngram": ngramConfig(), "exact": exactConfig(), "filtered": filteredConfig()} {
		buf, v, _ := encode(t, name, cfg, testEntries(300, 8))
		cfg2, err := builder.ListConfig(v)
		if err != nil {
			t.Fatal(err)
		}
		again, _, err := builder.Encode(name, cfg2, viewEntries(v), builder.Options{Source: "unit-test"})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, again) {
			t.Fatalf("%s: rebuilding from the artifact's records does not reproduce it", name)
		}
	}
}

func TestBuilderRefuses(t *testing.T) {
	good := testEntries(10, 1)
	for name, tc := range map[string]struct {
		name    string
		cfg     engine.ListConfig
		entries []engine.Entry
	}{
		"invalid name":    {"Bad Name", exactConfig(), good},
		"no entries":      {"x", exactConfig(), nil},
		"empty id":        {"x", exactConfig(), append(slices.Clone(good), engine.Entry{ID: "", Keys: []string{"k"}})},
		"duplicate id":    {"x", exactConfig(), append(slices.Clone(good), good[3])},
		"invalid payload": {"x", exactConfig(), []engine.Entry{{ID: "a", Keys: []string{"k"}, Payload: json.RawMessage(`{"a":`)}}},
		"invalid config":  {"x", engine.ListConfig{Match: engine.MatchConfig{Mode: "fuzzy"}}, good},
		"oversize line":   {"x", exactConfig(), []engine.Entry{{ID: "a", Keys: []string{strings.Repeat("\x01", 200000)}}}},
		"non-UTF-8 id":    {"x", exactConfig(), []engine.Entry{{ID: "a\xff", Keys: []string{"k"}}}},
		"non-UTF-8 key":   {"x", exactConfig(), []engine.Entry{{ID: "a", Keys: []string{"k\xff"}}}},
	} {
		if _, _, err := builder.Encode(tc.name, tc.cfg, tc.entries, builder.Options{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestListConfigChecksAgainstEngine(t *testing.T) {
	c, err := builder.Prepare("x", ngramConfig(), testEntries(5, 1), builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(c *prepared.Contents){
		"config digest":   func(c *prepared.Contents) { c.ConfigDigest = strings.Repeat("0", 64) },
		"analyzer digest": func(c *prepared.Contents) { c.AnalyzerDigest = strings.Repeat("0", 64) },
		"unresolved": func(c *prepared.Contents) {
			c.ListConfig = bytes.Replace(c.ListConfig, []byte(`"threshold":0.6,`), nil, 1)
		},
		"unknown field": func(c *prepared.Contents) {
			c.ListConfig = bytes.Replace(c.ListConfig, []byte(`{`), []byte(`{"zz":1,`), 1)
		},
	} {
		cc := *c
		mut(&cc)
		buf, _, err := prepared.Encode(&cc)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		v, _, err := prepared.VerifyBytes(buf, prepared.LevelFull)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := builder.ListConfig(v); err == nil {
			t.Errorf("%s: engine-level config check accepted it", name)
		}
	}
}

// TestNameRuleMatchesStore pins prepared.ValidListName to the store's own
// (unexported) list-name rule.
func TestNameRuleMatchesStore(t *testing.T) {
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	names := []string{"a", "0", "list", "my-list_2", strings.Repeat("a", 64), strings.Repeat("a", 65), "", "-a", "_a", "A", "a.b", "a b", "../x", "ä", "a/b", "products"}
	for _, n := range names {
		_, err := st.CreateList(n, exactConfig())
		var ce *engine.ConfigError
		storeOK := err == nil
		if err != nil && !errors.As(err, &ce) {
			t.Fatalf("%q: unexpected store error %v", n, err)
		}
		if prepared.ValidListName(n) != storeOK {
			t.Fatalf("%q: prepared says %v, store says %v (%v)", n, prepared.ValidListName(n), storeOK, err)
		}
	}
}

func TestWritePublishes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.kurn")
	res, err := builder.Write(path, "out", exactConfig(), testEntries(40, 2), builder.Options{Source: "s", CreatedAt: "2026-09-29T00:00:00Z", InputSHA256: strings.Repeat("ab", 32), Collapsed: 2})
	if err != nil {
		t.Fatal(err)
	}
	ver, err := prepared.Verify(path, prepared.LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	if ver.BaseID != res.BaseID || ver.ContainerSHA256 != res.ContainerSHA256 || res.Name != "out" || res.Mode != "exact" || res.Entries != 40 {
		t.Fatalf("result %+v, verified %+v", res, ver)
	}
	info, err := prepared.Inspect(path)
	if err != nil || info.Name != "out" || info.BaseID != res.BaseID {
		t.Fatalf("inspect %+v, %v", info, err)
	}
	buf, _ := prepared.ReadFile(path)
	v, _ := prepared.NewView(buf)
	if b := v.Build(); b.Source != "s" || b.CreatedAt != "2026-09-29T00:00:00Z" || b.Collapsed != 2 || b.InputSHA256 != strings.Repeat("ab", 32) {
		t.Fatalf("provenance %+v", b)
	}
	if _, err := builder.Write(path, "out", exactConfig(), testEntries(40, 2), builder.Options{}); !errors.Is(err, prepared.ErrExists) {
		t.Fatalf("second write: %v", err)
	}
}
