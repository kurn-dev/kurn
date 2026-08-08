package engine_test

// Filter admission-charge pins: zero when no filter is present, scaling
// with paths (and levels for exact), and the no-filter charge untouched.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestFilterChargePins(t *testing.T) {
	l, err := engine.NewList("charge", filterableCfg())
	if err != nil {
		t.Fatal(err)
	}
	var entries []engine.Entry
	for i := 0; i < 500; i++ {
		entries = append(entries, pEntry(fmt.Sprintf("p%d", i), fmt.Sprintf("key %d", i%7), "SDN"))
	}
	if err := l.Replace(entries); err != nil {
		t.Fatal(err)
	}

	unfiltered, err := l.PrepareFilteredQuery("key 1", engine.QueryOpts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain := l.PrepareQuery("key 1", engine.QueryOpts{})
	if unfiltered.Cost() != plain.Cost() {
		t.Fatalf("nil-filter charge %d != unfiltered %d — filter term must be zero when absent",
			unfiltered.Cost(), plain.Cost())
	}

	one, err := l.PrepareFilteredQuery("key 1", engine.QueryOpts{}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	if one.Cost() <= plain.Cost() {
		t.Fatal("one-path filtered charge does not exceed the unfiltered charge")
	}
	// Path-count scaling: the charge model caps declarations at 8, so a
	// two-declaration list proves the term scales with paths.
	cfg2 := filterableCfg()
	cfg2.Filterable = append(cfg2.Filterable, engine.FilterField{Name: "kind", Path: "kind"})
	l2, err := engine.NewList("charge2", cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if err := l2.Replace(entries); err != nil {
		t.Fatal(err)
	}
	two, err := l2.PrepareFilteredQuery("key 1", engine.QueryOpts{}, map[string]string{"program": "SDN", "kind": "x"})
	if err != nil {
		t.Fatal(err)
	}
	one2, err := l2.PrepareFilteredQuery("key 1", engine.QueryOpts{}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	if two.Cost() <= one2.Cost() {
		t.Fatal("two-path charge does not exceed one-path charge")
	}

	// Exact-value pins, derived from fixture facts: the filtered delta is
	// filterScanPerByte × payloadBytes + filterPerPathOrd × nPaths × ords
	// per segment (× levels for exact). The fixture is 500 entries with
	// 17-byte payloads and no tombstones or overlay.
	payloadBytes := int64(500) * int64(len(pEntry("p", "k", "SDN").Payload))
	var ords int64 = 500
	exprCharge := func(levels int64, values ...string) int64 {
		var charge, scalarBytes int64
		for _, value := range values {
			bytes := int64(len(value) + 2) // canonical JSON string bytes
			charge += 128 + 8*bytes
			scalarBytes += bytes
		}
		charge += ords * levels * (6*int64(len(values)) + scalarBytes)
		return charge
	}
	wantScanDelta := func(nPaths int64) int64 {
		return payloadBytes + 192*nPaths*ords // scan constants pinned: 1, 192
	}
	if want := wantScanDelta(1) + exprCharge(1, "SDN"); one.Cost()-plain.Cost() != want {
		t.Fatalf("one-path delta %d, formula says %d", one.Cost()-plain.Cost(), want)
	}
	if want := wantScanDelta(2) - wantScanDelta(1) + exprCharge(1, "x"); two.Cost()-one2.Cost() != want {
		t.Fatalf("second-path delta %d, formula says %d", two.Cost()-one2.Cost(), want)
	}

	// Max-path scaling: a list with eight declarations accepts eight
	// paths, and the charge must scale with the path count every time.
	cfg8 := filterableCfg()
	for i := 2; i <= 8; i++ {
		cfg8.Filterable = append(cfg8.Filterable, engine.FilterField{Name: fmt.Sprintf("f%d", i), Path: "f"})
	}
	l8, err := engine.NewList("charge8", cfg8)
	if err != nil {
		t.Fatal(err)
	}
	if err := l8.Replace(entries); err != nil {
		t.Fatal(err)
	}
	f8 := map[string]string{"program": "SDN"}
	for i := 2; i <= 8; i++ {
		f8[fmt.Sprintf("f%d", i)] = "x"
	}
	max, err := l8.PrepareFilteredQuery("key 1", engine.QueryOpts{}, f8)
	if err != nil {
		t.Fatal(err)
	}
	one8, err := l8.PrepareFilteredQuery("key 1", engine.QueryOpts{}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	if max.Cost() <= one8.Cost() {
		t.Fatal("eight-path charge does not exceed one-path charge")
	}
	if want := wantScanDelta(8) - wantScanDelta(1) + 7*exprCharge(1, "x"); max.Cost()-one8.Cost() != want {
		t.Fatalf("eight-path delta over one-path %d, formula says %d", max.Cost()-one8.Cost(), want)
	}

	// Exact mode: the delta scales with walked levels as well as paths.
	// "sub.deep.example.com" with parent_domain walks 3 levels; flat walks 1.
	xcfg := filterableCfg()
	xcfg.Match = engine.MatchConfig{Mode: "exact", Fallback: "parent_domain"}
	xl, err := engine.NewList("chargeexact", xcfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := xl.Replace(entries); err != nil {
		t.Fatal(err)
	}
	xplain := xl.PrepareQuery("sub.deep.example.com", engine.QueryOpts{})
	deep, err := xl.PrepareFilteredQuery("sub.deep.example.com", engine.QueryOpts{}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	if want := 3*wantScanDelta(1) + exprCharge(3, "SDN"); deep.Cost()-xplain.Cost() != want {
		t.Fatalf("exact 3-level delta %d, formula says %d", deep.Cost()-xplain.Cost(), want)
	}
	xcfg2 := xcfg
	xcfg2.Match.Fallback = ""
	xl2, err := engine.NewList("chargeexact2", xcfg2)
	if err != nil {
		t.Fatal(err)
	}
	if err := xl2.Replace(entries); err != nil {
		t.Fatal(err)
	}
	xplain2 := xl2.PrepareQuery("sub.deep.example.com", engine.QueryOpts{})
	flat, err := xl2.PrepareFilteredQuery("sub.deep.example.com", engine.QueryOpts{}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	if want := wantScanDelta(1) + exprCharge(1, "SDN"); flat.Cost()-xplain2.Cost() != want {
		t.Fatalf("exact 1-level delta %d, formula says %d", flat.Cost()-xplain2.Cost(), want)
	}
}

// Filtered-path allocation pin: the scanner is zero-alloc; a filtered
// query adds only the predicate closure(s) — assertion at the unfiltered
// hot-path ceiling of 60 (measured ~35).
func TestFilteredQueryAllocs(t *testing.T) {
	l, err := engine.NewList("allocs", filterableCfg())
	if err != nil {
		t.Fatal(err)
	}
	var entries []engine.Entry
	for i := 0; i < 200; i++ {
		entries = append(entries, pEntry(fmt.Sprintf("p%d", i), fmt.Sprintf("dana kovak %d", i%5), "SDN"))
	}
	if err := l.Replace(entries); err != nil {
		t.Fatal(err)
	}
	p, err := l.PrepareFilteredQuery("dana kovak 1", engine.QueryOpts{}, map[string]string{"program": "SDN"})
	if err != nil {
		t.Fatal(err)
	}
	p.Execute(context.Background()) // warm pools
	allocs := testing.AllocsPerRun(50, func() {
		p.Execute(context.Background())
	})
	if allocs > 60 {
		t.Fatalf("filtered query allocates %.0f per run, over the 60 ceiling", allocs)
	}
	t.Logf("filtered allocs/run: %.0f", allocs)
}

// The compiled filter must be detached from caller mutation.
func TestFilterCompileClonesCallerData(t *testing.T) {
	l, err := engine.NewList("clone", filterableCfg())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{pEntry("p1", "dana kovak", "SDN")}); err != nil {
		t.Fatal(err)
	}
	filter := map[string]string{"program": "SDN"}
	p, err := l.PrepareFilteredQuery("dana kovak", engine.QueryOpts{}, filter)
	if err != nil {
		t.Fatal(err)
	}
	filter["program"] = "CHANGED"
	got, _, err := p.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !json.Valid(got[0].Payload) || got[0].EntryID != "p1" {
		t.Fatalf("caller mutation after prepare changed the outcome: %+v", got)
	}
}

func TestTypedFilterAlternativeAndScalarCharge(t *testing.T) {
	l, err := engine.NewList("typed-charge", filterableCfg())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{pEntry("p1", "dana kovak", "SDN")}); err != nil {
		t.Fatal(err)
	}
	oneExpr, err := engine.ParseTypedFilter([]byte(`{"program":"SDN"}`))
	if err != nil {
		t.Fatal(err)
	}
	var raw strings.Builder
	raw.WriteString(`{"program":{"in":["SDN"`)
	for i := 1; i < 64; i++ {
		fmt.Fprintf(&raw, `,"v%02d"`, i)
	}
	raw.WriteString(`]}}`)
	maxExpr, err := engine.ParseTypedFilter([]byte(raw.String()))
	if err != nil {
		t.Fatal(err)
	}
	one, err := l.PrepareTypedFilteredQuery("dana kovak", engine.QueryOpts{}, oneExpr)
	if err != nil {
		t.Fatal(err)
	}
	max, err := l.PrepareTypedFilteredQuery("dana kovak", engine.QueryOpts{}, maxExpr)
	if err != nil {
		t.Fatal(err)
	}
	// Snapshot/path work is identical. The delta is the extra 63 compiled
	// alternatives plus their canonical string bytes (63 × five bytes for
	// the quoted vNN values), at the pinned expression constants.
	want := int64(63*128 + 63*5*8 + 63*(6+5)) // one ordinal in this fixture
	if got := max.Cost() - one.Cost(); got != want {
		t.Fatalf("max-alternative charge delta %d, want %d", got, want)
	}

	max.Execute(context.Background()) // warm pools
	allocs := testing.AllocsPerRun(50, func() {
		if _, _, err := max.Execute(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 60 {
		t.Fatalf("64-alternative typed execution allocates %.0f per run, over the 60 ceiling", allocs)
	}
}

func TestTypedNumberScannerAllocs(t *testing.T) {
	cfg := filterableCfg()
	cfg.Filterable = []engine.FilterField{{Name: "n", Path: "n"}}
	l, err := engine.NewList("typed-number-allocs", cfg)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]engine.Entry, 1000)
	for i := range entries {
		entries[i] = engine.Entry{
			ID: fmt.Sprintf("n%04d", i), Keys: []string{"dana kovak"},
			Payload: json.RawMessage(`{"n":1.00}`),
		}
	}
	if err := l.Replace(entries); err != nil {
		t.Fatal(err)
	}
	f, err := engine.ParseTypedFilter([]byte(`{"n":1e0}`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := l.PrepareTypedFilteredQuery("dana kovak", engine.QueryOpts{}, f)
	if err != nil {
		t.Fatal(err)
	}
	p.Execute(context.Background())
	allocs := testing.AllocsPerRun(25, func() {
		if _, _, err := p.Execute(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 60 {
		t.Fatalf("1000 numeric predicate evaluations allocate %.0f per run, over the 60 ceiling", allocs)
	}
}
