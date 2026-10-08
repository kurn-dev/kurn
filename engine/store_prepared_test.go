package engine_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kurn-dev/kurn/bench"
	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/prepared"
	"github.com/kurn-dev/kurn/engine/prepared/builder"
)

// Tests of the prepared (artifact) loader: OpenPrepared must serve exactly
// what a mutable store built from the same entries and configuration
// serves, under the same version stamp, and must never change.

const prepListName = "watch"

// prepCase is one configuration with its entries, queries and filters.
type prepCase struct {
	name    string
	cfg     engine.ListConfig
	entries func() []engine.Entry // fresh slices on every call
	queries []string
	filters []map[string]string // map filters (filtered cases only)
	typed   []string            // typed filter JSON (filtered cases only)
}

var prepWords = []string{"anna", "smith", "acme", "holdings", "zeta", "ltd", "müller", "o'brien", "jöhn", "doe", "alpha", "beta", "ivanov", "petrov"}

// prepPayload returns entry i's payload: absent, JSON null, compact, or
// non-compact with escapable characters, so canonicalization is exercised.
func prepPayload(i int) json.RawMessage {
	programs := []string{"SDN", "EU", "UN"}
	countries := []string{"IR", "RU", "KP", "SY"}
	switch i % 6 {
	case 0:
		return nil
	case 1:
		return json.RawMessage(`null`)
	case 2:
		return json.RawMessage(fmt.Sprintf(`{"program":%q,"meta":{"country":%q},"n":%d}`, programs[i%3], countries[i%4], i))
	case 3:
		return json.RawMessage(fmt.Sprintf(`{ "program" : [ %q , "UN" ] ,  "meta" : { "country" : %q }, "note": "<b>&co\u2028" }`, programs[i%3], countries[(i/2)%4]))
	case 4:
		return json.RawMessage(fmt.Sprintf(`{"program":%q,"meta":{"country":%q}}`, programs[(i/3)%3], countries[i%4]))
	default:
		return json.RawMessage(`{"n":1.50,"t":true}`)
	}
}

// prepNameEntries covers null keys, empty keys, keyless entries (every key
// analyzes to nothing) and payload-less entries.
func prepNameEntries(n int) []engine.Entry {
	out := make([]engine.Entry, n)
	for i := range out {
		e := &out[i]
		e.ID = fmt.Sprintf("p-%05d", (i*7919)%100000)
		w := func(k int) string { return prepWords[(i*k+k)%len(prepWords)] }
		switch {
		case i%11 == 5:
			e.Keys = nil // JSON null
		case i%13 == 6:
			e.Keys = []string{} // JSON []
		case i%7 == 3:
			e.Keys = []string{"!!!", "Mr.", "  "} // keyless: all analyze to ""
		default:
			up := func(s string) string { return strings.ToUpper(s[:1]) + s[1:] }
			e.Keys = []string{up(w(1)) + " " + up(w(3))}
			if i%3 == 0 {
				e.Keys = append(e.Keys, "Dr. "+w(5)+" "+w(2), "!!!")
			}
		}
		e.Payload = prepPayload(i)
	}
	return out
}

func prepDomainEntries(n int) []engine.Entry {
	out := make([]engine.Entry, n)
	tlds := []string{"com", "co.uk", "de", "org"}
	for i := range out {
		e := &out[i]
		e.ID = fmt.Sprintf("d-%05d", (i*104729)%100000)
		switch {
		case i%11 == 5:
			e.Keys = nil
		case i%13 == 6:
			e.Keys = []string{}
		case i%7 == 3:
			e.Keys = []string{"   "} // keyless under trim
		default:
			e.Keys = []string{fmt.Sprintf("Example%d.%s", i%40, tlds[i%4])}
			if i%5 == 0 {
				e.Keys = append(e.Keys, fmt.Sprintf("bücher%d.de", i%9), fmt.Sprintf("shop.example%d.com", i%40))
			}
		}
		e.Payload = prepPayload(i)
	}
	return out
}

var prepFilterable = []engine.FilterField{{Name: "program", Path: "program"}, {Name: "country", Path: "meta.country"}}

func prepCases() []prepCase {
	nameQueries := []string{"", "   ", "!!!", "anna smith", "Smith Anna", "acme holdings", "acme holdngs", "Dr. Zeta Beta", "müller", "Muller Doe", "o'brien alpha", "petrov ivanov", "ivan", "zzzz qqqq", "jöhn doe", "Ltd"}
	domainQueries := []string{"", "  ", "example3.com", "example4.com", "EXAMPLE8.COM", "a.b.example12.com", "shop.example10.com", "x.shop.example10.com", "bücher2.de", "xn--bcher2-3ya.de",
		"example5.co.uk", "deep.example9.co.uk", "example2.de", "www.example6.de", "example3.org", "a.example7.org", "com", "nothing.invalid"}
	filters := []map[string]string{{"program": "SDN"}, {"program": "EU", "country": "RU"}, {"country": "IR"}, {"undeclared": "x"}, {"program": "nope"}}
	typed := []string{`{"program":{"in":["SDN","EU"]}}`, `{"country":"KP"}`, `{"program":"UN","country":{"in":["IR","SY"]}}`, `{"undeclared":"x"}`}
	nameCfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true},
	}
	domainCfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "domain"},
		Match:    engine.MatchConfig{Mode: "exact", Fallback: "parent_domain"},
	}
	nameFiltered := nameCfg
	nameFiltered.Filterable = prepFilterable
	domainFiltered := domainCfg
	domainFiltered.Filterable = prepFilterable
	domainFiltered.Match.Fallback = ""
	return []prepCase{
		{name: "ngram", cfg: nameCfg, entries: func() []engine.Entry { return prepNameEntries(300) }, queries: nameQueries},
		{name: "exact", cfg: domainCfg, entries: func() []engine.Entry { return prepDomainEntries(300) }, queries: domainQueries},
		// Filtered cases: every entry has a payload, so filters evaluate
		// rather than abort ...
		{name: "ngram-filtered", cfg: nameFiltered, entries: func() []engine.Entry { return prepFillPayloads(prepNameEntries(300)) }, queries: nameQueries, filters: filters, typed: typed},
		{name: "exact-filtered", cfg: domainFiltered, entries: func() []engine.Entry { return prepFillPayloads(prepDomainEntries(300)) }, queries: domainQueries, filters: filters, typed: typed},
		// ... and with payload-less entries, whose evaluation is an error
		// the prepared list must reproduce exactly.
		{name: "ngram-filtered-gaps", cfg: nameFiltered, entries: func() []engine.Entry { return prepNameEntries(300) }, queries: nameQueries, filters: filters, typed: typed},
		{name: "exact-filtered-gaps", cfg: domainFiltered, entries: func() []engine.Entry { return prepDomainEntries(300) }, queries: domainQueries, filters: filters, typed: typed},
	}
}

// prepFillPayloads gives every payload-less entry a payload.
func prepFillPayloads(es []engine.Entry) []engine.Entry {
	for i := range es {
		if es[i].Payload == nil {
			es[i].Payload = json.RawMessage(`{"program":"UN","meta":{"country":"SY"}}`)
		}
	}
	return es
}

var prepOpts = []engine.QueryOpts{{}, {Threshold: -1, TopK: -1}, {TopK: 2}, {Threshold: 0.3}}

// prepAnswer is one recorded answer.
type prepAnswer struct {
	Kind     string
	Q        string
	Opts     engine.QueryOpts
	Filter   any                 `json:",omitempty"`
	Cands    []engine.Candidate  `json:",omitempty"`
	Payloads []string            `json:",omitempty"` // raw payload bytes, uncompacted by marshaling
	Version  string              `json:",omitempty"`
	Err      string              `json:",omitempty"`
	Stats    *engine.FilterStats `json:",omitempty"`
	Cost     int64               `json:",omitempty"`
}

func prepRaw(cs []engine.Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c.Payload)
	}
	return out
}

// prepAnswers records every query path's answers and every read accessor
// of l as JSON. Filtered admission costs are left out: see
// prepFilteredCosts.
func prepAnswers(t testing.TB, l *engine.List, c prepCase) []byte {
	t.Helper()
	ctx := context.Background()
	var out []prepAnswer
	for _, q := range c.queries {
		for _, o := range prepOpts {
			cs, ver := l.QueryVersioned(ctx, q, o)
			out = append(out, prepAnswer{Kind: "query", Q: q, Opts: o, Cands: cs, Payloads: prepRaw(cs), Version: ver})
			p := l.PrepareQuery(q, o)
			cs, ver = p.Execute(ctx)
			out = append(out, prepAnswer{Kind: "prepared", Q: q, Opts: o, Cands: cs, Payloads: prepRaw(cs), Version: ver, Cost: p.Cost()})
		}
	}
	// Batch: every check pinned first, then executed, as a batch request
	// does.
	var batch []*engine.PreparedQuery
	for _, q := range c.queries {
		batch = append(batch, l.PrepareQuery(q, engine.QueryOpts{}))
	}
	for i, p := range batch {
		cs, ver := p.Execute(ctx)
		out = append(out, prepAnswer{Kind: "batch", Q: c.queries[i], Cands: cs, Payloads: prepRaw(cs), Version: ver, Cost: p.Cost()})
	}
	for _, q := range c.queries {
		for _, f := range c.filters {
			cs, ver, err := l.QueryFilteredCtx(ctx, q, engine.QueryOpts{Threshold: -1, TopK: -1}, f)
			a := prepAnswer{Kind: "filtered", Q: q, Filter: f, Cands: cs, Payloads: prepRaw(cs), Version: ver}
			if err != nil {
				a.Err = err.Error()
			}
			out = append(out, a)
		}
		var tbatch []*engine.FilteredQuery
		var traw []string
		for _, raw := range c.typed {
			tf, err := engine.ParseTypedFilter([]byte(raw))
			if err != nil {
				t.Fatalf("typed filter %s: %v", raw, err)
			}
			cs, ver, err := l.QueryTypedFilteredCtx(ctx, q, engine.QueryOpts{}, tf)
			a := prepAnswer{Kind: "typed", Q: q, Filter: raw, Cands: cs, Payloads: prepRaw(cs), Version: ver}
			if err != nil {
				a.Err = err.Error()
			}
			out = append(out, a)
			if p, err := l.PrepareTypedFilteredQuery(q, engine.QueryOpts{Threshold: -1, TopK: -1}, tf); err == nil {
				tbatch = append(tbatch, p)
				traw = append(traw, raw)
			}
		}
		for i, p := range tbatch {
			cs, ver, st, err := p.ExecuteStats(ctx)
			a := prepAnswer{Kind: "typed-batch", Q: q, Filter: traw[i], Cands: cs, Payloads: prepRaw(cs), Version: ver, Stats: &st}
			if err != nil {
				a.Err = err.Error()
			}
			out = append(out, a)
		}
	}
	entries, overlay, tomb := l.Stats()
	dropped, keyless := l.BuildStats()
	facts := map[string]any{
		"name": l.Name(), "version": l.Version(), "config_digest": l.ConfigDigest(), "config": l.Config(),
		"status": l.Status(), "key_count": l.KeyCount(), "stats": []int{entries, overlay, tomb, dropped, keyless, l.UnindexedEntries()},
		"live": l.LiveEntries(), "scratch": l.ScratchBytes(), "scratch_for": l.ScratchBytesFor(5),
	}
	raw, err := json.Marshal(struct {
		Answers []prepAnswer
		Facts   map[string]any
	}{out, facts})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// prepFilteredCosts returns the admission charges of every filtered
// prepared query.
func prepFilteredCosts(t testing.TB, l *engine.List, c prepCase) []int64 {
	var out []int64
	for _, q := range c.queries {
		for _, f := range c.filters {
			if p, err := l.PrepareFilteredQuery(q, engine.QueryOpts{}, f); err == nil {
				out = append(out, p.Cost())
			}
		}
		for _, raw := range c.typed {
			tf, _ := engine.ParseTypedFilter([]byte(raw))
			if p, err := l.PrepareTypedFilteredQuery(q, engine.QueryOpts{}, tf); err == nil {
				out = append(out, p.Cost())
			}
		}
	}
	return out
}

// prepWriteArtifact publishes an artifact of c into a fresh directory and
// returns its path.
func prepWriteArtifact(t testing.TB, c prepCase) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), prepListName+".kart")
	if _, err := builder.Write(path, prepListName, c.cfg, c.entries(), builder.Options{Source: "test"}); err != nil {
		t.Fatalf("builder.Write: %v", err)
	}
	return path
}

func prepOpen(t testing.TB, path string, opts engine.PreparedOptions) *engine.Store {
	t.Helper()
	st, err := engine.OpenPrepared(path, opts)
	if err != nil {
		t.Fatalf("OpenPrepared: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func prepList(t testing.TB, st *engine.Store) *engine.List {
	t.Helper()
	l, ok := st.List(prepListName)
	if !ok {
		t.Fatalf("list %q missing", prepListName)
	}
	return l
}

func prepCopyFile(t testing.TB, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// prepMutable builds c into a mutable store, closes it, and returns the
// in-process stamp, the in-process filtered costs, and the reopened store
// (engine.Open with the index cache present).
func prepMutable(t *testing.T, c prepCase) (string, *engine.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList(prepListName, c.cfg); err != nil {
		t.Fatal(err)
	}
	ver, err := st.ReplaceVersioned(prepListName, c.entries())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, prepListName, "base.idx")); err != nil {
		t.Fatalf("index cache missing before reopen: %v", err)
	}
	re, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { re.Close() })
	if len(re.Skipped) != 0 || len(re.Quarantined) != 0 {
		t.Fatalf("reopen skipped %v quarantined %v", re.Skipped, re.Quarantined)
	}
	return ver, re, dir
}

// prepBundle builds c as a bundle (a temporary store's list files, as the
// bundle builder produces them), ships it into a fresh data directory and
// serves it through ReloadList, the bundle publish path.
func prepBundle(t *testing.T, c prepCase) *engine.Store {
	t.Helper()
	build := t.TempDir()
	st, err := engine.Open(build)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("bundle", c.cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("bundle", c.entries()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	serve := t.TempDir()
	sv, err := engine.Open(serve)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sv.Close() })
	lp := filepath.Join(serve, prepListName)
	if err := os.MkdirAll(lp, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"config.json", "base.jsonl", "base.idx"} {
		prepCopyFile(t, filepath.Join(build, "bundle", f), filepath.Join(lp, f))
	}
	if _, _, err := sv.ReloadListVersioned(prepListName); err != nil {
		t.Fatal(err)
	}
	return sv
}

// prepRebuilt reopens a copy of a mutable store's list without its index
// cache, so engine.Open rebuilds the index from base.jsonl.
func prepRebuilt(t *testing.T, mutableDir string) *engine.Store {
	t.Helper()
	dir := t.TempDir()
	lp := filepath.Join(dir, prepListName)
	if err := os.MkdirAll(lp, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"config.json", "base.jsonl"} {
		prepCopyFile(t, filepath.Join(mutableDir, prepListName, f), filepath.Join(lp, f))
	}
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// (a) Oracle equivalence: every answer and every read accessor of the
// prepared list is byte-identical to the mutable store's, reopened from
// its index cache, to the same content served as a bundle, and to a full
// rebuild; and all carry one version stamp.
func TestPreparedOracleEquivalence(t *testing.T) {
	for _, c := range prepCases() {
		t.Run(c.name, func(t *testing.T) {
			stamp, mut, mutDir := prepMutable(t, c)
			bun := prepBundle(t, c)
			reb := prepRebuilt(t, mutDir)
			path := prepWriteArtifact(t, c)
			pst := prepOpen(t, path, engine.PreparedOptions{})
			pl := prepList(t, pst)

			want := prepAnswers(t, prepList(t, mut), c)
			prepCheckCoverage(t, want, c)
			for name, st := range map[string]*engine.Store{"bundle": bun, "rebuilt": reb, "prepared": pst} {
				got := prepAnswers(t, prepList(t, st), c)
				if !bytes.Equal(got, want) {
					t.Errorf("%s answers differ from the mutable store's:\n%s", name, prepFirstDiff(got, want))
				}
				if v := prepList(t, st).Version(); v != stamp {
					t.Errorf("%s version %s, the mutable store stamped %s", name, v, stamp)
				}
			}
			info := pst.Prepared()
			n := len(c.entries())
			if wantStamp := fmt.Sprintf("%s@%d+j0+c%s", info.BaseID, n, pl.ConfigDigest()); stamp != wantStamp {
				t.Errorf("stamp %s, want %s (verified base ID continuity)", stamp, wantStamp)
			}
			if info.Level != prepared.LevelFull || !info.IdentitiesRecomputed || info.Loader != engine.LoaderHeap {
				t.Errorf("info %+v", info)
			}
			if info.ConfigDigest != pl.ConfigDigest() {
				t.Errorf("declared config digest %s, list %s", info.ConfigDigest, pl.ConfigDigest())
			}
			if !pst.ReadOnly() || len(pst.DamagedLists()) != 0 || len(pst.Lists()) != 1 {
				t.Errorf("ReadOnly %v damaged %v lists %d", pst.ReadOnly(), pst.DamagedLists(), len(pst.Lists()))
			}
			if pl.BaseNgram() != nil || pl.BaseExact() != nil {
				t.Error("a read-only list handed out its borrowed index")
			}
			// Filtered admission charges equal a list whose base was built
			// by Replace (the rebuilt store): the prepared loader totals
			// payload bytes as buildSegment does.
			if len(c.filters) > 0 {
				gotC, wantC := prepFilteredCosts(t, pl, c), prepFilteredCosts(t, prepList(t, reb), c)
				if fmt.Sprint(gotC) != fmt.Sprint(wantC) {
					t.Errorf("filtered costs %v, a Replace-built base charges %v", gotC, wantC)
				}
			}
			if pst.ReadOnlyRefusals() != 0 {
				t.Errorf("refusals %d after reads only", pst.ReadOnlyRefusals())
			}
		})
	}
}

// prepCheckCoverage keeps the oracle from passing vacuously: queries must
// find candidates, and filtered cases without payload gaps must answer
// filtered queries with candidates rather than errors.
func prepCheckCoverage(t *testing.T, raw []byte, c prepCase) {
	t.Helper()
	var rec struct{ Answers []prepAnswer }
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, a := range rec.Answers {
		if a.Err == "" && len(a.Cands) > 0 {
			counts[a.Kind]++
		}
	}
	kinds := []string{"query", "prepared", "batch"}
	if len(c.filters) > 0 && !strings.HasSuffix(c.name, "-gaps") {
		kinds = append(kinds, "filtered", "typed", "typed-batch")
	}
	for _, k := range kinds {
		if counts[k] < 3 {
			t.Fatalf("oracle coverage: only %d %s answers with candidates (%v)", counts[k], k, counts)
		}
	}
}

func prepFirstDiff(a, b []byte) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	lo := max(0, i-200)
	return fmt.Sprintf("at byte %d:\n got: %s\nwant: %s", i, a[lo:min(len(a), i+200)], b[lo:min(len(b), i+200)])
}

// (i) Payload aliasing contract: a result's payload is exactly the stored
// payload bytes of that record.
func TestPreparedPayloadIsStoredBytes(t *testing.T) {
	for _, c := range prepCases() {
		t.Run(c.name, func(t *testing.T) {
			path := prepWriteArtifact(t, c)
			l := prepList(t, prepOpen(t, path, engine.PreparedOptions{}))
			buf, err := prepared.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			v, _, err := prepared.VerifyBytes(buf, prepared.LevelFull)
			if err != nil {
				t.Fatal(err)
			}
			seen, empty := 0, 0
			for _, q := range c.queries {
				for _, cand := range l.Query(q, engine.QueryOpts{Threshold: -1, TopK: -1}) {
					ord, ok := v.Lookup(cand.EntryID)
					if !ok {
						t.Fatalf("result %q not in the artifact", cand.EntryID)
					}
					if !bytes.Equal(cand.Payload, v.Payload(ord)) || (cand.Payload == nil) != (v.Payload(ord) == nil) {
						t.Fatalf("%q: payload %q, stored %q", cand.EntryID, cand.Payload, v.Payload(ord))
					}
					if cand.Payload == nil {
						empty++
					}
					seen++
				}
			}
			if seen == 0 || (empty == 0 && strings.HasPrefix(c.name, "ngram") && !strings.Contains(c.name, "filtered")) {
				t.Fatalf("checked %d results, %d payload-less", seen, empty)
			}
		})
	}
}

// (h) A mutable store is not read-only and has no prepared info.
func TestPreparedMutableStoreUnaffected(t *testing.T) {
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.ReadOnly() || st.Prepared() != nil || st.ReadOnlyRefusals() != 0 {
		t.Fatalf("mutable store: ReadOnly %v Prepared %v refusals %d", st.ReadOnly(), st.Prepared(), st.ReadOnlyRefusals())
	}
	if _, err := st.CreateList("m", engine.ListConfig{Match: engine.MatchConfig{Mode: "exact"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("m", []engine.Entry{{ID: "a", Keys: []string{"x"}}}); err != nil {
		t.Fatal(err)
	}
	l, _ := st.List("m")
	if l.BaseNgram() != nil || l.BaseExact() != nil {
		t.Fatal("no base yet, want nil indexes")
	}
	if err := st.Compact("m"); err != nil {
		t.Fatal(err)
	}
	if l.BaseExact() == nil {
		t.Fatal("mutable list withholds its base index")
	}
	if st.ReadOnlyRefusals() != 0 {
		t.Fatal("mutable store counted refusals")
	}
}

// (f) LevelStructure must be asked for, and is reported; the engine's
// identity recomputation does not depend on the level. The mapped loader
// is not available yet; unknown loaders and levels are errors.
func TestPreparedLevelsAndLoaders(t *testing.T) {
	c := prepCases()[0]
	path := prepWriteArtifact(t, c)
	full := prepOpen(t, path, engine.PreparedOptions{})
	st := prepOpen(t, path, engine.PreparedOptions{Verify: prepared.LevelStructure})
	info := st.Prepared()
	if info.Level != prepared.LevelStructure || !info.IdentitiesRecomputed {
		t.Fatalf("structure-level info %+v", info)
	}
	if info.BaseID != "" || info.ContainerSHA256 != "" {
		t.Fatalf("structure level claims identities: %+v", info)
	}
	if got, want := prepList(t, st).Version(), prepList(t, full).Version(); got != want {
		t.Fatalf("structure-level stamp %s, full %s", got, want)
	}
	if !bytes.Equal(prepAnswers(t, prepList(t, st), c), prepAnswers(t, prepList(t, full), c)) {
		t.Fatal("structure-level answers differ")
	}
	// Prepared returns a copy.
	info.Name = "changed"
	if st.Prepared().Name != prepListName {
		t.Fatal("Prepared returned shared state")
	}

	for _, tc := range []struct {
		opts engine.PreparedOptions
		is   error
	}{
		{engine.PreparedOptions{Loader: engine.Loader(7)}, nil},
		{engine.PreparedOptions{Verify: prepared.Level(9)}, nil},
	} {
		st, err := engine.OpenPrepared(path, tc.opts)
		if err == nil || st != nil {
			t.Fatalf("%+v: store %v err %v, want a refusal", tc.opts, st, err)
		}
		if tc.is != nil && !errors.Is(err, tc.is) {
			t.Fatalf("%+v: %v is not %v", tc.opts, err, tc.is)
		}
	}
}

// prepSnapshotDir records every file under dir with its mode and SHA-256.
func prepSnapshotDir(t testing.TB, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rec := fi.Mode().String()
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			rec += " " + hex.EncodeToString(sum[:])
		}
		out[p] = rec
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// prepSections returns the section table: kind → (offset, length), plus
// each entry's byte offset in the table.
func prepSections(buf []byte) map[uint32][3]uint64 {
	n := int(binary.LittleEndian.Uint32(buf[16:]))
	out := map[uint32][3]uint64{}
	for i := 0; i < n; i++ {
		e := buf[24+64*i:]
		out[binary.LittleEndian.Uint32(e)] = [3]uint64{binary.LittleEndian.Uint64(e[8:]), binary.LittleEndian.Uint64(e[16:]), uint64(24 + 64*i)}
	}
	return out
}

// prepRehash recomputes every section digest and then the header digest,
// so a patched artifact passes all of the format's hashing (a local
// equivalent of the prepared package's test helper).
func prepRehash(buf []byte) {
	n := int(binary.LittleEndian.Uint32(buf[16:]))
	for _, s := range prepSections(buf) {
		sum := sha256.Sum256(buf[s[0] : s[0]+s[1]])
		copy(buf[s[2]+24:s[2]+56], sum[:])
	}
	hl := 24 + 64*n + 32
	sum := sha256.Sum256(buf[:hl-32])
	copy(buf[hl-32:hl], sum[:])
}

// prepEncode returns c's artifact bytes in an aligned buffer.
func prepEncode(t testing.TB, c prepCase) []byte {
	t.Helper()
	buf, _, err := builder.Encode(prepListName, c.cfg, c.entries(), builder.Options{Source: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return buf
}

// prepPatchSection replaces the first occurrence of old in the section of
// the given kind with new (same length).
func prepPatchSection(t testing.TB, buf []byte, kind uint32, old, new string) {
	t.Helper()
	if len(old) != len(new) {
		t.Fatalf("patch changes length: %q → %q", old, new)
	}
	s := prepSections(buf)[kind]
	body := buf[s[0] : s[0]+s[1]]
	i := bytes.Index(body, []byte(old))
	if i < 0 {
		t.Fatalf("%q not in section kind %d: %s", old, kind, body)
	}
	copy(body[i:], new)
}

func prepWriteBytes(t testing.TB, buf []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "patched.kart")
	if err := os.WriteFile(path, buf, 0o444); err != nil {
		t.Fatal(err)
	}
	return path
}

// 100k sanity numbers for the lane report: OpenPrepared against engine.Open
// with its index cache, in-process, a few repetitions. Not a benchmark;
// opt-in via KURN_PREPARED_SANITY=1.
func TestPreparedOpenSanity100k(t *testing.T) {
	if os.Getenv("KURN_PREPARED_SANITY") == "" {
		t.Skip("set KURN_PREPARED_SANITY=1")
	}
	const n = 100_000
	entries := func() []engine.Entry {
		es := bench.Generate(42, n)
		for i := range es {
			es[i].Payload = json.RawMessage(fmt.Sprintf(`{"program":"SDN","n":%d}`, i))
		}
		return es
	}
	cfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true},
	}
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList(prepListName, cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace(prepListName, entries()); err != nil {
		t.Fatal(err)
	}
	st.Close()
	path := filepath.Join(t.TempDir(), "a.kart")
	if _, err := builder.Write(path, prepListName, cfg, entries(), builder.Options{}); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	var baseSize, idxSize int64
	if fi2, err := os.Stat(filepath.Join(dir, prepListName, "base.jsonl")); err == nil {
		baseSize = fi2.Size()
	}
	if fi2, err := os.Stat(filepath.Join(dir, prepListName, "base.idx")); err == nil {
		idxSize = fi2.Size()
	}
	t.Logf("artifact %d bytes; base.jsonl %d + base.idx %d bytes", fi.Size(), baseSize, idxSize)

	measure := func(name string, open func() (*engine.Store, error)) {
		for rep := 0; rep < 3; rep++ {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			t0 := time.Now()
			s, err := open()
			d := time.Since(t0)
			if err != nil {
				t.Fatal(err)
			}
			runtime.GC()
			runtime.ReadMemStats(&after)
			l, _ := s.List(prepListName)
			t.Logf("%-22s rep %d: %8.1f ms  HeapAlloc Δ %6.1f MiB  TotalAlloc Δ %6.1f MiB  (version %.20s…)", name, rep, float64(d.Microseconds())/1000,
				float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/(1<<20), float64(after.TotalAlloc-before.TotalAlloc)/(1<<20), l.Version())
			s.Close()
			runtime.KeepAlive(s)
		}
	}
	// Attribution: the buffer plus the View and its FrozenView index alone,
	// without the engine's per-entry structures.
	for rep := 0; rep < 2; rep++ {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		buf, err := prepared.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		v, _, err := prepared.VerifyBytes(buf, prepared.LevelStructure)
		if err != nil {
			t.Fatal(err)
		}
		idx, err := v.NgramIndex()
		if err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		t.Logf("buffer+View+NgramIndex rep %d: HeapAlloc Δ %6.1f MiB  TotalAlloc Δ %6.1f MiB  (%d grams)", rep,
			float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/(1<<20), float64(after.TotalAlloc-before.TotalAlloc)/(1<<20), v.GramCount())
		runtime.KeepAlive(idx)
	}
	measure("engine.Open (cache)", func() (*engine.Store, error) { return engine.Open(dir) })
	measure("OpenPrepared full", func() (*engine.Store, error) { return engine.OpenPrepared(path, engine.PreparedOptions{}) })
	measure("OpenPrepared structure", func() (*engine.Store, error) {
		return engine.OpenPrepared(path, engine.PreparedOptions{Verify: prepared.LevelStructure})
	})
}
