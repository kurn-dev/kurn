package engine_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/prepared"
	"github.com/kurn-dev/kurn/engine/prepared/builder"
)

// Tests of the trusted policy (prepared.LevelTrusted) through the heap
// loader. The mapped loader's trusted matrix runs in child processes (see
// store_prepared_mapped_trusted_test.go).

// prepCaseNamed returns the prepCases entry with the given name.
func prepCaseNamed(t testing.TB, name string) prepCase {
	for _, c := range prepCases() {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("no case %q", name)
	return prepCase{}
}

// prepSetSectionDigest writes section kind's actual digest into its table
// entry, leaving the header digest stale.
func prepSetSectionDigest(buf []byte, kind uint32) {
	s := prepSections(buf)[kind]
	sum := sha256.Sum256(buf[s[0] : s[0]+s[1]])
	copy(buf[s[2]+24:s[2]+56], sum[:])
}

func prepCopy(b []byte) []byte {
	c := prepared.AlignedBuffer(len(b))
	copy(c, b)
	return c
}

// A trusted open reports its policy honestly and answers exactly as a full
// open of the same valid artifact: same answers, same stamp.
func TestPreparedTrustedOpen(t *testing.T) {
	for _, c := range prepCases() {
		t.Run(c.name, func(t *testing.T) {
			path := prepWriteArtifact(t, c)
			full := prepOpen(t, path, engine.PreparedOptions{})
			tr := prepOpen(t, path, engine.PreparedOptions{Verify: prepared.LevelTrusted})
			st := prepOpen(t, path, engine.PreparedOptions{Verify: prepared.LevelStructure})
			fi, ti, si := full.Prepared(), tr.Prepared(), st.Prepared()

			if fi.Level != prepared.LevelFull || !fi.ContentVerified || !fi.HeaderDigestVerified || fi.StampBasis != "verified-base-id" || fi.BaseID == "" || fi.ContainerSHA256 == "" {
				t.Fatalf("full info %+v", fi)
			}
			if ti.Level != prepared.LevelTrusted || ti.Level.String() != "trusted" || ti.ContentVerified || !ti.HeaderDigestVerified ||
				ti.BaseID != "" || ti.ContainerSHA256 != "" || ti.DeclaredBaseID != fi.BaseID ||
				ti.StampBasis != "declared-base-id" || !ti.IdentitiesRecomputed || ti.Loader != engine.LoaderHeap {
				t.Fatalf("trusted info %+v", ti)
			}
			if si.Level != prepared.LevelStructure || si.HeaderDigestVerified || si.ContentVerified || si.StampBasis != engine.StampBasisDeclared {
				t.Fatalf("structure info %+v", si)
			}
			want := prepAnswers(t, prepList(t, full), c)
			if got := prepAnswers(t, prepList(t, tr), c); !bytes.Equal(got, want) {
				t.Fatalf("trusted answers differ from full:\n%s", prepFirstDiff(got, want))
			}
			if prepList(t, tr).Version() != prepList(t, full).Version() {
				t.Fatal("trusted stamp differs")
			}
			if len(c.filters) > 0 {
				if a, b := prepFilteredCosts(t, prepList(t, tr), c), prepFilteredCosts(t, prepList(t, full), c); fmt.Sprint(a) != fmt.Sprint(b) {
					t.Fatalf("filtered costs %v, full %v", a, b)
				}
			}
			// Prepared returns a copy.
			ti.Level, ti.StampBasis = prepared.LevelFull, engine.StampBasisVerified
			if tr.Prepared().Level != prepared.LevelTrusted || tr.Prepared().StampBasis != engine.StampBasisDeclared {
				t.Fatal("Prepared returned shared state")
			}
		})
	}
}

// Empty payloads: an artifact whose records include payload-less entries
// opens under both full and trusted with identical query answers (checked
// for every case above); here the fixture is shown to contain them.
func TestPreparedTrustedEmptyPayloads(t *testing.T) {
	c := prepCases()[0]
	tr := prepOpen(t, prepWriteArtifact(t, c), engine.PreparedOptions{Verify: prepared.LevelTrusted})
	empty := 0
	for _, e := range prepList(t, tr).LiveEntries() {
		if len(e.Payload) == 0 {
			empty++
		}
	}
	if empty == 0 {
		t.Fatal("no payload-less entries in the fixture")
	}
}

// The assurance difference through the engine: a records byte altered
// without updating its recorded digest opens under trusted (answers come
// from the altered bytes) and is refused under full; with the altered
// digest written into the table and the header digest stale, both refuse.
// No full failure falls back to trusted.
func TestPreparedTrustedAssuranceDifference(t *testing.T) {
	c := prepCaseNamed(t, "ngram-filtered")
	buf, target := prepAlterPayload(t, c)
	path := prepWriteBytes(t, buf)

	st, err := engine.OpenPrepared(path, engine.PreparedOptions{})
	if st != nil || !errors.Is(err, prepared.ErrIntegrity) || !strings.Contains(err.Error(), "section kind 3") {
		t.Fatalf("full: store %v err %v", st, err)
	}
	tr := prepOpen(t, path, engine.PreparedOptions{Verify: prepared.LevelTrusted})
	if !prepServesAltered(prepList(t, tr), target) {
		t.Fatalf("trusted answers for %q do not come from the altered bytes", target.Keys[0])
	}
	if info := tr.Prepared(); info.ContentVerified || info.BaseID != "" {
		t.Fatalf("trusted claims content: %+v", info)
	}

	prepSetSectionDigest(buf, prepared.KindRecords)
	path2 := prepWriteBytes(t, buf)
	for _, lvl := range []prepared.Level{prepared.LevelFull, prepared.LevelTrusted} {
		st, err := engine.OpenPrepared(path2, engine.PreparedOptions{Verify: lvl})
		if st != nil || !errors.Is(err, prepared.ErrIntegrity) || !strings.Contains(err.Error(), "header") {
			t.Fatalf("%v with a stale header digest: store %v err %v", lvl, st, err)
		}
	}
}

// prepAlterPayload encodes c and changes one byte of the payload of an
// entry that a query of its first key finds ("SDN" → "SDX"), leaving
// every recorded digest untouched: a structurally valid content change.
func prepAlterPayload(t testing.TB, c prepCase) ([]byte, engine.Entry) {
	t.Helper()
	var target engine.Entry
	for _, e := range c.entries() {
		if len(e.Keys) > 0 && strings.Contains(e.Keys[0], " ") && bytes.Contains(e.Payload, []byte(`"program":"SDN"`)) {
			target = e
			break
		}
	}
	buf := prepEncode(t, c)
	rs := prepSections(buf)[prepared.KindRecords]
	body := buf[rs[0] : rs[0]+rs[1]]
	at := bytes.Index(body, []byte(target.ID))
	if target.ID == "" || at < 0 {
		t.Fatal("no target entry")
	}
	j := bytes.Index(body[at:], []byte(`"program":"SDN"`))
	if j < 0 {
		t.Fatal("target payload not found")
	}
	copy(body[at+j:], `"program":"SDX"`)
	return buf, target
}

// prepServesAltered reports whether a query of target's first key returns
// target with the altered payload.
func prepServesAltered(l *engine.List, target engine.Entry) bool {
	for _, cd := range l.Query(target.Keys[0], engine.QueryOpts{Threshold: -1, TopK: -1}) {
		if cd.EntryID == target.ID && bytes.Contains(cd.Payload, []byte(`"program":"SDX"`)) {
			return true
		}
	}
	return false
}

// No fallback: a failed open at either serving level returns its error
// and no store; nothing is retried at another level.
func TestPreparedTrustedNoFallback(t *testing.T) {
	c := prepCases()[0]
	ok := prepEncode(t, c)
	// Content-altered: full refuses, and returns no store.
	b := prepCopy(ok)
	s := prepSections(b)[prepared.KindRecords]
	body := b[s[0] : s[0]+s[1]]
	i := bytes.LastIndex(body, []byte("Smith"))
	if i < 0 {
		t.Fatal("no key to alter")
	}
	body[i] = 'S' ^ 0x20 // "smith": a structurally valid content change
	altered := prepWriteBytes(t, b)
	st, err := engine.OpenPrepared(altered, engine.PreparedOptions{})
	if st != nil || !errors.Is(err, prepared.ErrIntegrity) {
		t.Fatalf("full: store %v err %v", st, err)
	}
	// A metadata digest failure refuses trusted too, with no store.
	m := prepCopy(ok)
	cs := prepSections(m)[prepared.KindConfig]
	m[cs[2]+30] ^= 1 // a wrong recorded config digest, header rehashed
	prepRehashHeader(m)
	bad := prepWriteBytes(t, m)
	for _, lvl := range []prepared.Level{prepared.LevelFull, prepared.LevelTrusted} {
		st, err := engine.OpenPrepared(bad, engine.PreparedOptions{Verify: lvl})
		if st != nil || !errors.Is(err, prepared.ErrIntegrity) || !strings.Contains(err.Error(), "section kind 1") {
			t.Fatalf("%v: store %v err %v", lvl, st, err)
		}
	}
}

// prepRehashHeader recomputes only the header and table digest.
func prepRehashHeader(buf []byte) {
	n := int(binary.LittleEndian.Uint32(buf[16:]))
	hl := 24 + 64*n + 32
	sum := sha256.Sum256(buf[:hl-32])
	copy(buf[hl-32:hl], sum[:])
}

// The payload rule through the engine: non-empty invalid JSON is refused
// under trusted (ErrMalformed) and full; valid non-canonical JSON opens
// under trusted and is refused under full.
func TestPreparedTrustedPayloadRule(t *testing.T) {
	c := prepCaseNamed(t, "ngram-filtered")
	invalid := prepEncode(t, c)
	prepPatchSection(t, invalid, prepared.KindRecords, `{"program":"SDN"`, `["program":"SDN"`)
	prepRehash(invalid)
	p := prepWriteBytes(t, invalid)
	if st, err := engine.OpenPrepared(p, engine.PreparedOptions{Verify: prepared.LevelTrusted}); st != nil || !errors.Is(err, prepared.ErrMalformed) || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("trusted, invalid JSON: store %v err %v", st, err)
	}
	if st, err := engine.OpenPrepared(p, engine.PreparedOptions{}); st != nil || err == nil {
		t.Fatalf("full, invalid JSON: store %v err %v", st, err)
	}

	noncanon := prepEncode(t, c)
	prepPatchSection(t, noncanon, prepared.KindRecords, `{"program":"SDN"`, `{ "program":"SD"`)
	prepRehash(noncanon)
	p = prepWriteBytes(t, noncanon)
	tr, err := engine.OpenPrepared(p, engine.PreparedOptions{Verify: prepared.LevelTrusted})
	if err != nil {
		t.Fatalf("trusted, valid non-canonical JSON: %v", err)
	}
	defer tr.Close()
	if st, err := engine.OpenPrepared(p, engine.PreparedOptions{}); st != nil || !errors.Is(err, prepared.ErrIntegrity) || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("full, non-canonical: store %v err %v", st, err)
	}
}

// Read-only guarantees hold under trusted.
func TestPreparedTrustedReadOnly(t *testing.T) {
	c := prepCases()[2]
	st := prepOpen(t, prepWriteArtifact(t, c), engine.PreparedOptions{Verify: prepared.LevelTrusted})
	l := prepList(t, st)
	want := prepAnswers(t, l, c)
	errCalls, noops := prepMutators(st, l, prepListName)
	for what, call := range errCalls {
		if err := call(); !errors.Is(err, engine.ErrReadOnly) {
			t.Errorf("%s: %v", what, err)
		}
	}
	for _, call := range noops {
		call()
	}
	if st.ReadOnlyRefusals() != uint64(len(errCalls)+len(noops)) || !bytes.Equal(prepAnswers(t, l, c), want) {
		t.Fatal("read-only guarantees broken under trusted")
	}
}

// auditPad is a canonical placeholder payload whose bytes are replaced in
// place (same length, padded with JSON whitespace) by arbitrary payloads.
var auditPad = `{"program":"SDN","meta":{"country":"IR"},"pad":"` + strings.Repeat("x", 400) + `"}`

// auditVariants are valid JSON, mostly non-canonical, stressing the
// payload scanner behind filtered queries.
var auditVariants = []string{
	`{"program":["SDN","EU"],"meta":{"country":"RU"}}`,
	`{"program":5,"meta":{"country":["IR"]}}`,
	`{"program":"SDN","program":"EU"}`,
	`{"meta":{"country":"IR","country":"KP"}}`,
	`[1,2,3]`,
	`"just a string"`,
	`null`,
	`true`,
	`123456789012345678901234567890e999`,
	`{"meta":null}`,
	`{"meta":"flat"}`,
	`{"meta":{"country":{"deep":[[[[{}]]]]}}}`,
	`{"program":"SDN","meta":{"country":"IR"}}`,
	`{"program":"<b>&amp; ","meta":{"country":"IR"}}`,
	`  {  "program" : "SDN" , "meta" : { "country" : "IR" }  }  `,
	`{"program":{"in":["SDN"]}}`,
	`{"":"","program":""}`,
	strings.Repeat(`[`, 90) + strings.Repeat(`]`, 90),
	`{"program":"SDN\"escaped\\","meta":{"country":"I\/R"}}`,
	`{"program":1e400}`,
}

// auditArtifact builds an artifact of c's configuration in which every
// payload is one of variants, in turn, padded to a common length.
func auditArtifact(t *testing.T, c prepCase, variants []string, rehash bool) []byte {
	t.Helper()
	es := c.entries()
	for i := range es {
		es[i].Payload = json.RawMessage(auditPad)
	}
	buf, _, err := builder.Encode(prepListName, c.cfg, es, builder.Options{Source: "audit"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range es {
		v := variants[i%len(variants)]
		if len(v) > len(auditPad) {
			t.Fatalf("variant too long: %d", len(v))
		}
		prepPatchSection(t, buf, prepared.KindRecords, auditPad, v+strings.Repeat(" ", len(auditPad)-len(v)))
	}
	if rehash {
		prepRehash(buf)
	}
	return buf
}

// auditRun drives every filtered and unfiltered query path over l; any
// panic fails the test. It returns how many filtered calls answered and
// how many returned errors.
func auditRun(t *testing.T, l *engine.List, c prepCase, encode bool) (answered, errored int) {
	t.Helper()
	ctx := context.Background()
	all := engine.QueryOpts{Threshold: -1, TopK: -1}
	count := func(cs []engine.Candidate, err error) {
		if err != nil {
			errored++
			return
		}
		answered++
		// Response encoding: a served payload must encode.
		if _, err := json.Marshal(cs); encode && err != nil {
			t.Fatalf("a result does not encode: %v", err)
		}
	}
	for _, q := range c.queries {
		cs := l.Query(q, all)
		count(cs, nil)
		for _, f := range c.filters {
			cs, _, err := l.QueryFilteredCtx(ctx, q, all, f)
			count(cs, err)
			if p, err := l.PrepareFilteredQuery(q, all, f); err == nil {
				cs, _, err := p.Execute(ctx)
				count(cs, err)
				cs, _, _, err = p.ExecuteStats(ctx)
				count(cs, err)
			}
		}
		for _, raw := range c.typed {
			tf, err := engine.ParseTypedFilter([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			cs, _, err := l.QueryTypedFilteredCtx(ctx, q, all, tf)
			count(cs, err)
			if p, err := l.PrepareTypedFilteredQuery(q, all, tf); err == nil {
				cs, _, _, err := p.ExecuteStats(ctx)
				count(cs, err)
			}
		}
	}
	return answered, errored
}

// Decoder audit: non-canonical and oddly shaped payloads reach every
// filtered query path of a trusted store and yield answers or errors,
// never panics, and every served payload encodes. Invalid JSON is refused
// at a trusted open; served at the diagnostic structure level, it yields
// errors on the filtered paths, never panics.
func TestPreparedTrustedDecoderAudit(t *testing.T) {
	for _, name := range []string{"ngram-filtered", "exact-filtered"} {
		t.Run(name, func(t *testing.T) {
			c := prepCaseNamed(t, name)
			buf := auditArtifact(t, c, auditVariants, true)
			path := prepWriteBytes(t, buf)
			if st, err := engine.OpenPrepared(path, engine.PreparedOptions{}); st != nil || !errors.Is(err, prepared.ErrIntegrity) {
				t.Fatalf("full accepted non-canonical payloads: %v", err)
			}
			tr := prepOpen(t, path, engine.PreparedOptions{Verify: prepared.LevelTrusted})
			a, e := auditRun(t, prepList(t, tr), c, true)
			if a == 0 || e == 0 {
				t.Fatalf("audit coverage: %d answered, %d errors", a, e)
			}
			t.Logf("trusted: %d filtered/unfiltered calls answered, %d returned errors", a, e)

			invalid := []string{`{"program":`, `{"program":"SDN"`, `not json`, `{"a":1}}`, "\xff\xfe", `[1,]`, `{"program":"S` + "\x01" + `N"}`, `"unterminated`}
			ibuf := auditArtifact(t, c, invalid, true)
			ipath := prepWriteBytes(t, ibuf)
			if st, err := engine.OpenPrepared(ipath, engine.PreparedOptions{Verify: prepared.LevelTrusted}); st != nil || !errors.Is(err, prepared.ErrMalformed) {
				t.Fatalf("trusted accepted invalid JSON: %v", err)
			}
			ss := prepOpen(t, ipath, engine.PreparedOptions{Verify: prepared.LevelStructure})
			// Not encoding-checked: the diagnostic level serves payloads
			// that do not encode, which is exactly what the trusted payload
			// rule refuses at open.
			a, e = auditRun(t, prepList(t, ss), c, false)
			if e == 0 {
				t.Fatal("invalid payloads produced no filtered errors")
			}
			t.Logf("structure (diagnostic) with invalid JSON: %d answered, %d errors", a, e)
		})
	}
}

// Decoder audit: the ID map built at load. A record ID altered in a way
// that stays structurally valid (still unique and in order) is served
// under trusted from the altered bytes, consistently; one that breaks the
// ID order is refused before any exposure.
func TestPreparedTrustedIDMapAudit(t *testing.T) {
	c := prepCases()[0]
	es := c.entries()
	ids := make([]string, len(es))
	for i, e := range es {
		ids[i] = e.ID
	}
	sort.Strings(ids)
	maxID, second := ids[len(ids)-1], ids[len(ids)-2]

	b := prepEncode(t, c)
	s := prepSections(b)[prepared.KindRecords]
	body := b[s[0] : s[0]+s[1]]
	i := bytes.Index(body, []byte(maxID))
	if i < 0 {
		t.Fatal("max ID not in the records")
	}
	body[i] = 'q' // "p-…" → "q-…": still the greatest, still unique
	path := prepWriteBytes(t, b)
	tr := prepOpen(t, path, engine.PreparedOptions{Verify: prepared.LevelTrusted})
	got := map[string]bool{}
	for _, e := range prepList(t, tr).LiveEntries() {
		got[e.ID] = true
	}
	if !got["q"+maxID[1:]] || got[maxID] || len(got) != len(es) {
		t.Fatalf("ID map does not reflect the altered ID consistently (%d entries)", len(got))
	}
	if st, err := engine.OpenPrepared(path, engine.PreparedOptions{}); st != nil || !errors.Is(err, prepared.ErrIntegrity) {
		t.Fatalf("full: %v", err)
	}

	d := prepEncode(t, c)
	s = prepSections(d)[prepared.KindRecords]
	body = d[s[0] : s[0]+s[1]]
	i = bytes.Index(body, []byte(maxID))
	copy(body[i:], second) // a duplicate ID
	if st, err := engine.OpenPrepared(prepWriteBytes(t, d), engine.PreparedOptions{Verify: prepared.LevelTrusted}); st != nil || !errors.Is(err, prepared.ErrMalformed) {
		t.Fatalf("duplicate ID under trusted: %v", err)
	}
}

// Decoder audit: exact.Restore over an altered postings array. A run's
// first posting moved down by one (still in range and strictly ascending)
// is served under trusted and refused under full; an out-of-range posting
// is refused under trusted before exposure.
func TestPreparedTrustedExactRestoreAudit(t *testing.T) {
	c := prepCaseNamed(t, "exact-filtered")
	n := uint32(len(c.entries()))
	for _, tc := range []struct {
		name  string
		value func(old uint32) uint32
		ok    bool
	}{
		{"in range", func(old uint32) uint32 { return old - 1 }, true},
		{"out of range", func(uint32) uint32 { return n + 7 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := prepEncode(t, c)
			s := prepSections(b)[prepared.KindExact]
			sec := b[s[0] : s[0]+s[1]]
			k := int(binary.LittleEndian.Uint64(sec[0:]))
			runs := sec[24+4*k:]
			post := sec[24+8*k:]
			j, at := -1, 0
			for x := 0; x < k; x++ {
				if binary.LittleEndian.Uint32(post[4*at:]) > 0 {
					j = x
					break
				}
				at += int(binary.LittleEndian.Uint32(runs[4*x:]))
			}
			if j < 0 {
				t.Fatal("no run whose first posting is above zero")
			}
			old := binary.LittleEndian.Uint32(post[4*at:])
			binary.LittleEndian.PutUint32(post[4*at:], tc.value(old))
			path := prepWriteBytes(t, b)
			tr, err := engine.OpenPrepared(path, engine.PreparedOptions{Verify: prepared.LevelTrusted})
			if tc.ok {
				if err != nil {
					t.Fatalf("trusted: %v", err)
				}
				defer tr.Close()
				auditRun(t, prepList(t, tr), c, true)
				if st, err := engine.OpenPrepared(path, engine.PreparedOptions{}); st != nil || !errors.Is(err, prepared.ErrIntegrity) {
					t.Fatalf("full: %v", err)
				}
			} else if tr != nil || !errors.Is(err, prepared.ErrMalformed) {
				t.Fatalf("out-of-range posting under trusted: store %v err %v", tr, err)
			}
		})
	}
}
