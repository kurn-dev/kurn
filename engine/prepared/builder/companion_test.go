package builder_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/prepared"
	"github.com/kurn-dev/kurn/engine/prepared/builder"
)

func companionConfig() engine.ListConfig {
	c := ngramConfig()
	c.Match.ExactCompanion = true
	return c
}

// sectionTable decodes the artifact's section table: kind, offset, length.
type tableEntry struct {
	kind        uint32
	off, length uint64
}

func sectionTable(buf []byte) []tableEntry {
	n := int(binary.LittleEndian.Uint32(buf[16:]))
	out := make([]tableEntry, n)
	for i := range out {
		e := buf[24+64*i:]
		out[i] = tableEntry{binary.LittleEndian.Uint32(e), binary.LittleEndian.Uint64(e[8:]), binary.LittleEndian.Uint64(e[16:])}
	}
	return out
}

// rehashAll recomputes every section digest and the header digest, as a
// producer of a self-consistent but altered artifact would.
func rehashAll(buf []byte) {
	n := int(binary.LittleEndian.Uint32(buf[16:]))
	for i := 0; i < n; i++ {
		e := buf[24+64*i:]
		off, l := binary.LittleEndian.Uint64(e[8:]), binary.LittleEndian.Uint64(e[16:])
		sum := sha256.Sum256(buf[off : off+l])
		copy(e[24:56], sum[:])
	}
	hl := 24 + n*64 + 32
	sum := sha256.Sum256(buf[:hl-32])
	copy(buf[hl-32:hl], sum[:])
}

// loadedCompanionList installs an artifact's ngram and companion indexes
// as the prepared loader does.
func loadedCompanionList(t *testing.T, v *prepared.View) *engine.List {
	t.Helper()
	cfg, err := builder.ListConfig(v)
	if err != nil {
		t.Fatal(err)
	}
	l, err := engine.NewList(v.Config().Name, cfg)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := v.NgramIndex()
	if err != nil {
		t.Fatal(err)
	}
	companion, err := v.ExactCompanionIndex()
	if err != nil {
		t.Fatal(err)
	}
	b := v.Build()
	if err := l.ReplaceWithIndexesInfo(viewEntries(v), idx, companion, &engine.IndexBuildInfo{Entries: b.Entries, DroppedKeys: b.DroppedKeys, KeylessEntries: b.KeylessEntries}); err != nil {
		t.Fatal(err)
	}
	return l
}

func anyOfIDs(t *testing.T, l *engine.List, terms ...string) string {
	t.Helper()
	p, err := l.PrepareExactAnyOf(terms, engine.QueryOpts{})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := p.Execute(context.Background())
	var s []string
	for _, x := range c {
		s = append(s, x.EntryID+"|"+x.Key)
	}
	return strings.Join(s, ",")
}

func TestExactCompanionArtifactRoundTrip(t *testing.T) {
	buf, v, ver := encode(t, "names", companionConfig(), testEntries(300, 7))
	if !ver.ExactCompanion || !v.Config().ExactCompanion || ver.Mode != "ngram" || ver.Minor != 1 {
		t.Fatalf("verified: %+v", ver)
	}
	kinds := map[uint32]bool{}
	for _, s := range sectionTable(buf) {
		kinds[s.kind] = true
	}
	if !kinds[prepared.KindNgram] || !kinds[prepared.KindExactCompanion] || kinds[prepared.KindExact] || len(kinds) != 6 {
		t.Fatalf("section kinds: %v", kinds)
	}

	// The stored companion is the engine's own index over the same records.
	ref, err := engine.NewList("names", companionConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := ref.Replace(testEntries(300, 7)); err != nil {
		t.Fatal(err)
	}
	want := ref.BaseExactCompanion()
	got, err := v.ExactCompanionIndex()
	if err != nil {
		t.Fatal(err)
	}
	wk, gk := want.SortedKeys(), got.SortedKeys()
	if len(wk) != len(gk) || len(wk) == 0 {
		t.Fatalf("companion keys: %d vs %d", len(gk), len(wk))
	}
	for i := range wk {
		if wk[i] != gk[i] {
			t.Fatalf("key %d: %q vs %q", i, gk[i], wk[i])
		}
		a, b := want.Lookup(wk[i]), got.Lookup(gk[i])
		if len(a) != len(b) {
			t.Fatalf("key %q: runs %v vs %v", wk[i], b, a)
		}
		for j := range a {
			if a[j] != b[j] {
				t.Fatalf("key %q: runs %v vs %v", wk[i], b, a)
			}
		}
	}

	// A list built over the artifact answers like the mutable one.
	l := loadedCompanionList(t, v)
	entries := testEntries(300, 7)
	for i := 0; i < 40; i++ {
		terms := append([]string{}, entries[i].Keys...)
		terms = append(terms, strings.ToUpper(entries[(i*7)%300].Keys[0]))
		if a, b := anyOfIDs(t, l, terms...), anyOfIDs(t, ref, terms...); a != b {
			t.Fatalf("terms %q: artifact %q, mutable %q", terms, a, b)
		}
	}
	for _, q := range queries(entries)[:20] {
		sameCandidates(t, "ngram "+q, l.Query(q, engine.QueryOpts{}), ref.Query(q, engine.QueryOpts{}))
	}

	// Inspect, trusted verification and the semantic rebuild all see it.
	path := filepath.Join(t.TempDir(), "names.kurn")
	if err := os.WriteFile(path, buf, 0o444); err != nil {
		t.Fatal(err)
	}
	info, err := prepared.Inspect(path)
	if err != nil || !info.ExactCompanion || info.Format != "KURNART1 1.1" {
		t.Fatalf("inspect: %+v %v", info, err)
	}
	if tv, err := prepared.Verify(path, prepared.LevelTrusted); err != nil || !tv.ExactCompanion {
		t.Fatalf("trusted: %+v %v", tv, err)
	}
	rep, err := builder.VerifySemanticBytes(buf)
	if err != nil || !rep.OK() || !rep.CompanionOK || !rep.IndexOK || rep.IndexKind != "ngram" {
		t.Fatalf("semantic: %v %+v", err, rep)
	}
	if n := len(rep.Sections); n != 6 || rep.Sections[5].Kind != "exact_companion" || !rep.Sections[5].OK {
		t.Fatalf("sections: %+v", rep.Sections)
	}

	// Write reports it too.
	res, err := builder.Write(filepath.Join(t.TempDir(), "w.kurn"), "names", companionConfig(), testEntries(50, 3), builder.Options{})
	if err != nil || !res.ExactCompanion {
		t.Fatalf("write: %+v %v", res, err)
	}
}

func TestExactCompanionEncodeRefusals(t *testing.T) {
	entries := testEntries(40, 1)
	with, err := builder.Prepare("names", companionConfig(), entries, builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if with.ExactCompanion == nil || with.Exact != nil || with.Ngram == nil {
		t.Fatal("Prepare did not produce exactly an ngram index and a companion")
	}
	without, err := builder.Prepare("names", ngramConfig(), entries, builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if without.ExactCompanion != nil {
		t.Fatal("a plain ngram list produced a companion")
	}
	missing := *with
	missing.ExactCompanion = nil
	if _, _, err := prepared.Encode(&missing); err == nil || !strings.Contains(err.Error(), "companion") {
		t.Fatalf("missing companion: %v", err)
	}
	extra := *without
	extra.ExactCompanion = with.ExactCompanion
	if _, _, err := prepared.Encode(&extra); err == nil || !strings.Contains(err.Error(), "companion") {
		t.Fatalf("unexpected companion: %v", err)
	}
	bad := exactConfig()
	bad.Match.ExactCompanion = true
	if _, err := builder.Prepare("codes", bad, entries, builder.Options{}); err == nil {
		t.Fatal("exact mode with exact_companion accepted")
	}
}

// A corrupted companion posting: refused at full verification by its
// digest, not seen at trusted and structure levels (a content section),
// and once the digests are recomputed it passes full verification but
// fails the semantic rebuild, which names the companion.
func TestExactCompanionCorruptionDetection(t *testing.T) {
	buf, v, _ := encode(t, "names", companionConfig(), testEntries(300, 7))
	var sec tableEntry
	for _, s := range sectionTable(buf) {
		if s.kind == prepared.KindExactCompanion {
			sec = s
		}
	}
	body := buf[sec.off : sec.off+sec.length]
	k := int(binary.LittleEndian.Uint64(body))
	runLens := body[24+4*k:]
	postings := body[24+8*k:]
	// A single-ordinal run can point at any other record without
	// breaking the section's structure.
	pIdx, found := 0, false
	for i := 0; i < k; i++ {
		rl := int(binary.LittleEndian.Uint32(runLens[4*i:]))
		if rl == 1 {
			found = true
			break
		}
		pIdx += rl
	}
	if !found {
		t.Fatal("no single-ordinal run to corrupt")
	}
	old := binary.LittleEndian.Uint32(postings[4*pIdx:])
	binary.LittleEndian.PutUint32(postings[4*pIdx:], (old+1)%uint32(v.Len()))

	if _, _, err := prepared.VerifyBytes(buf, prepared.LevelFull); !errors.Is(err, prepared.ErrIntegrity) {
		t.Fatalf("full verification of a corrupted companion: %v", err)
	}
	if _, _, err := prepared.VerifyBytes(buf, prepared.LevelTrusted); err != nil {
		t.Fatalf("trusted verification hashed the companion: %v", err)
	}
	if _, _, err := prepared.VerifyBytes(buf, prepared.LevelStructure); err != nil {
		t.Fatalf("structure: %v", err)
	}
	rehashAll(buf)
	if _, _, err := prepared.VerifyBytes(buf, prepared.LevelFull); err != nil {
		t.Fatalf("rehashed artifact must pass full verification: %v", err)
	}
	rep, err := builder.VerifySemanticBytes(buf)
	if !errors.Is(err, prepared.ErrIntegrity) || rep == nil || rep.OK() || rep.CompanionOK || !rep.IndexOK || rep.CompanionMismatch == "" {
		t.Fatalf("semantic: %v %+v", err, rep)
	}
	if !strings.Contains(err.Error(), "exact companion") {
		t.Fatalf("failure does not name the companion: %v", err)
	}

	// The companion section is required by the configuration: dropping it
	// (it is the last section) is malformed.
	tab := sectionTable(buf)
	last := tab[len(tab)-1]
	if last.kind != prepared.KindExactCompanion {
		t.Fatalf("last section kind %d", last.kind)
	}
	dropped := buf[:tab[len(tab)-2].off+tab[len(tab)-2].length]
	n := len(tab) - 1
	binary.LittleEndian.PutUint32(dropped[16:], uint32(n))
	oldHl, newHl := 24+(n+1)*64+32, 24+n*64+32
	for i := newHl; i < oldHl; i++ {
		dropped[i] = 0
	}
	// The zeroed bytes run into the alignment gap before section 0 only
	// if the table shrank past a 32-byte boundary; either way the gap
	// must be zero, which it now is.
	sum := sha256.Sum256(dropped[:newHl-32])
	copy(dropped[newHl-32:newHl], sum[:])
	if _, _, err := prepared.VerifyBytes(dropped, prepared.LevelStructure); !errors.Is(err, prepared.ErrMalformed) || !strings.Contains(err.Error(), "exact companion") {
		t.Fatalf("companion section dropped: %v", err)
	}
}
