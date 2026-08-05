package engine_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/analyzer"
	"github.com/kurn-dev/kurn/engine/artifact"
	"github.com/kurn-dev/kurn/engine/exact"
	"github.com/kurn-dev/kurn/engine/ngram"
)

func personCfg() engine.ListConfig {
	return engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, Threshold: 0.6, TopK: 100},
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("people", personCfg()); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("people", []engine.Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("people", []engine.Entry{{ID: "p2", Keys: []string{"Dana Kovak"}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete("people", "p1"); err != nil {
		t.Fatal(err)
	}

	// restart: journal must replay
	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := st2.List("people")
	if !ok {
		t.Fatal("list lost on restart")
	}
	if c := l.Query("dana kovak", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "p2" {
		t.Fatalf("replayed upsert: %+v", c)
	}
	if c := l.Query("marcus chen", engine.QueryOpts{}); len(c) != 0 {
		t.Fatalf("replayed delete: %+v", c)
	}
}

func TestStoreCompactThenRestart(t *testing.T) {
	dir := t.TempDir()
	st, _ := engine.Open(dir)
	st.CreateList("people", personCfg())
	st.Replace("people", []engine.Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}})
	st.Upsert("people", []engine.Entry{{ID: "p2", Keys: []string{"Dana Kovak"}}})
	if err := st.Compact("people"); err != nil {
		t.Fatal(err)
	}

	st2, _ := engine.Open(dir)
	l, _ := st2.List("people")
	entries, overlay, tombs := l.Stats()
	if entries != 2 || overlay != 0 || tombs != 0 {
		t.Fatalf("post-restart stats = %d,%d,%d want 2,0,0", entries, overlay, tombs)
	}
	if c := l.Query("dana kovak", engine.QueryOpts{}); len(c) != 1 {
		t.Fatalf("query after compact+restart: %+v", c)
	}
}

func mustQuery(t *testing.T, l *engine.List, q string) []engine.Candidate {
	t.Helper()
	return l.Query(q, engine.QueryOpts{})
}

// seedStore creates a compacted people list with p1+p2 and returns the dir.
func seedStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("people", personCfg()); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("people", []engine.Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("people", []engine.Entry{{ID: "p2", Keys: []string{"Dana Kovak"}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Compact("people"); err != nil {
		t.Fatal(err)
	}
	return dir
}

// cfgDigest returns the analyzer-spec digest the store will record in (and
// require from) an artifact for a list with cfg — test-crafted artifacts
// must carry it or the install path (correctly) rejects them.
func cfgDigest(t *testing.T, cfg engine.ListConfig) string {
	t.Helper()
	an, err := engine.ResolveAnalyzer(cfg.Analyzer)
	if err != nil {
		t.Fatal(err)
	}
	return engine.AnalyzerSpecDigest(an)
}

// TestStoreArtifactFastPathUsed proves Open installs the on-disk artifact

// baseIDOf computes base.jsonl's content identity the way readBase does:
// sha256 of the file bytes, 12 hex chars. Doctored-artifact tests must
// state the real identity or the install path (correctly) rebuilds.
func baseIDOf(t *testing.T, dir, list string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, list, "base.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// rather than rebuilding: base.idx is overwritten with a valid, config-
// matching index built from DIFFERENT keys, and after restart queries follow
// the doctored index, not base.jsonl's keys.
func TestStoreArtifactFastPathUsed(t *testing.T) {
	dir := t.TempDir()
	st, _ := engine.Open(dir)
	st.CreateList("people", personCfg())
	if err := st.Replace("people", []engine.Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}}); err != nil {
		t.Fatal(err)
	}

	b := ngram.NewBuilder(ngram.Config{Grams: []int{2, 3}, StripSpaces: true})
	b.Add(0, []string{"dana kovak"})
	if err := artifact.Save(filepath.Join(dir, "people", "base.idx"), b.Finish(), cfgDigest(t, personCfg()), artifact.BuildInfo{BaseID: baseIDOf(t, dir, "people"), Entries: 1}); err != nil {
		t.Fatal(err)
	}

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := st2.List("people")
	if c := mustQuery(t, l, "dana kovak"); len(c) != 1 || c[0].EntryID != "p1" {
		t.Fatalf("doctored artifact not used (rebuild happened?): %+v", c)
	}
	if c := mustQuery(t, l, "marcus chen"); len(c) != 0 {
		t.Fatalf("doctored artifact not used, base keys still indexed: %+v", c)
	}
}

// TestStoreCorruptArtifactFallsBack: a corrupt base.idx must not fail Open,
// and results must match a clean restart (full rebuild from base.jsonl).
func TestStoreCorruptArtifactFallsBack(t *testing.T) {
	dir := seedStore(t)
	idxPath := filepath.Join(dir, "people", "base.idx")
	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("compact did not write base.idx: %v", err)
	}
	if err := os.WriteFile(idxPath, []byte("KURNIDX1 this is garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("corrupt artifact failed startup: %v", err)
	}
	l, _ := st.List("people")
	if c := mustQuery(t, l, "dana kovak"); len(c) != 1 || c[0].EntryID != "p2" {
		t.Fatalf("fallback rebuild wrong: %+v", c)
	}
	if c := mustQuery(t, l, "marcus chen"); len(c) != 1 || c[0].EntryID != "p1" {
		t.Fatalf("fallback rebuild wrong: %+v", c)
	}
	if e, o, tb := l.Stats(); e != 2 || o != 0 || tb != 0 {
		t.Fatalf("fallback stats %d,%d,%d", e, o, tb)
	}
}

// TestStoreConfigChangeRebuilds: index-time config edited on disk (grams)
// must force a rebuild with the new config, not install the stale artifact.
func TestStoreConfigChangeRebuilds(t *testing.T) {
	dir := seedStore(t)
	cfgPath := filepath.Join(dir, "people", "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg engine.ListConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Match.Grams = []int{3}
	raw, _ = json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := st.List("people")
	idx := l.BaseNgram()
	if idx == nil {
		t.Fatal("no base index after rebuild")
	}
	// The stale artifact has grams [2,3]; a rebuild carries the new config.
	if got := idx.Cfg().Grams; len(got) != 1 || got[0] != 3 {
		t.Fatalf("stale artifact installed despite config change: grams %v", got)
	}
	if c := mustQuery(t, l, "dana kovak"); len(c) != 1 || c[0].EntryID != "p2" {
		t.Fatalf("query after config-change rebuild: %+v", c)
	}
}

// TestStoreMismatchedArtifactFallsBack: a valid artifact whose NumOrds
// exceeds base.jsonl's entry count must be rejected (ReplaceWithIndex
// validation) and fall back to a full build.
func TestStoreMismatchedArtifactFallsBack(t *testing.T) {
	dir := seedStore(t) // base.jsonl has 2 entries
	b := ngram.NewBuilder(ngram.Config{Grams: []int{2, 3}, StripSpaces: true})
	b.Add(0, []string{"marcus chen"})
	b.Add(1, []string{"dana kovak"})
	b.Add(2, []string{"rosa almeida"}) // 3 ords > 2 entries
	if err := artifact.Save(filepath.Join(dir, "people", "base.idx"), b.Finish(), cfgDigest(t, personCfg()), artifact.BuildInfo{}); err != nil {
		t.Fatal(err)
	}

	st, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("mismatched artifact failed startup: %v", err)
	}
	l, _ := st.List("people")
	// A rebuilt index has exactly 2 ordinals; the doctored artifact has 3.
	// (Query text is no discriminator here: the known-gram denominator can
	// score partial-overlap queries at 100, so inspect the index directly.)
	idx := l.BaseNgram()
	if idx == nil {
		t.Fatal("no base index after fallback")
	}
	if n := idx.NumOrds(); n != 2 {
		t.Fatalf("mismatched artifact was installed: NumOrds %d, want 2", n)
	}
	if c := mustQuery(t, l, "marcus chen"); len(c) != 1 || c[0].EntryID != "p1" {
		t.Fatalf("fallback rebuild wrong: %+v", c)
	}
	if c := mustQuery(t, l, "dana kovak"); len(c) != 1 || c[0].EntryID != "p2" {
		t.Fatalf("fallback rebuild wrong: %+v", c)
	}
}

func TestStoreCreateListNames(t *testing.T) {
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "../evil", "UPPER", "-lead", "_lead", "a b", "a/b", strings.Repeat("a", 65), "a.b"} {
		if _, err := st.CreateList(bad, personCfg()); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	for _, good := range []string{"a", "people", "a-b_c9", strings.Repeat("a", 64)} {
		if _, err := st.CreateList(good, personCfg()); err != nil {
			t.Errorf("name %q rejected: %v", good, err)
		}
	}
	// Invalid config must be rejected too.
	if _, err := st.CreateList("badcfg", engine.ListConfig{Match: engine.MatchConfig{Mode: "psychic"}}); err == nil {
		t.Error("invalid config accepted")
	}
}

// TestStoreRecreateWipes: CreateList on an existing name is PUT-replace —
// prior data files are wiped and the list is fresh, across restart too.
func TestStoreRecreateWipes(t *testing.T) {
	dir := seedStore(t)
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.Upsert("people", []engine.Entry{{ID: "p3", Keys: []string{"Rosa Almeida"}}}) // journal content
	if _, err := st.CreateList("people", personCfg()); err != nil {
		t.Fatal(err)
	}
	l, _ := st.List("people")
	if e, _, _ := l.Stats(); e != 0 {
		t.Fatalf("recreated list not empty: %d entries", e)
	}
	for _, f := range []string{"base.jsonl", "base.idx", "journal.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, "people", f)); !os.IsNotExist(err) {
			t.Errorf("%s not wiped (err=%v)", f, err)
		}
	}
	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l2, ok := st2.List("people")
	if !ok {
		t.Fatal("recreated list lost on restart")
	}
	if e, _, _ := l2.Stats(); e != 0 {
		t.Fatalf("recreated list has data after restart: %d entries", e)
	}
}

func TestStoreUnknownList(t *testing.T) {
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("ghost", []engine.Entry{{ID: "x", Keys: []string{"x"}}}); err == nil {
		t.Error("Upsert on unknown list succeeded")
	}
	if err := st.Delete("ghost", "x"); err == nil {
		t.Error("Delete on unknown list succeeded")
	}
	if err := st.Compact("ghost"); err == nil {
		t.Error("Compact on unknown list succeeded")
	}
	if err := st.Replace("ghost", nil); err == nil {
		t.Error("Replace on unknown list succeeded")
	}
	if _, ok := st.List("ghost"); ok {
		t.Error("List returned unknown list")
	}
}

// TestStorePayloadRoundTrip: payload journaled by Upsert survives restart
// byte-for-byte.
func TestStorePayloadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, _ := engine.Open(dir)
	st.CreateList("people", personCfg())
	payload := json.RawMessage(`{"rank":7,"tags":["x","y"],"note":"naïve — ütf8"}`)
	if err := st.Upsert("people", []engine.Entry{{ID: "p1", Keys: []string{"Marcus Chen"}, Payload: payload}}); err != nil {
		t.Fatal(err)
	}

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := st2.List("people")
	c := mustQuery(t, l, "marcus chen")
	if len(c) != 1 {
		t.Fatalf("candidates: %+v", c)
	}
	if !bytes.Equal(c[0].Payload, payload) {
		t.Fatalf("payload %s != %s", c[0].Payload, payload)
	}
}

// seedExactStore builds a store with a compacted 2-entry exact list (so
// base.jsonl and base.idx both exist) and returns its dir.
func seedExactStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "exact"},
	}
	if _, err := st.CreateList("codes", cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("codes", []engine.Entry{{ID: "c1", Keys: []string{"AA-1"}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("codes", []engine.Entry{{ID: "c2", Keys: []string{"BB-2"}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Compact("codes"); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestStoreExactList: exact-mode lists persist, save a base.idx artifact on
// Compact, and answer queries after restart.
func TestStoreExactList(t *testing.T) {
	dir := seedExactStore(t)
	if _, err := os.Stat(filepath.Join(dir, "codes", "base.idx")); err != nil {
		t.Errorf("compact did not write exact base.idx: %v", err)
	}

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := st2.List("codes")
	if c := mustQuery(t, l, "bb-2"); len(c) != 1 || c[0].EntryID != "c2" {
		t.Fatalf("exact query after restart: %+v", c)
	}
	if c := mustQuery(t, l, "aa-1"); len(c) != 1 || c[0].EntryID != "c1" {
		t.Fatalf("exact query after restart: %+v", c)
	}
}

// TestStoreExactArtifactFastPathUsed proves Open installs the on-disk exact
// artifact rather than rebuilding: base.idx is overwritten with a valid index
// built from DIFFERENT keys, and after restart queries follow the doctored
// index, not base.jsonl's keys.
func TestStoreExactArtifactFastPathUsed(t *testing.T) {
	dir := seedExactStore(t)
	b := exact.NewBuilder()
	b.Add(0, []string{"doctored-key"})
	doctored, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if err := artifact.SaveExact(filepath.Join(dir, "codes", "base.idx"), doctored, cfgDigest(t, exactCfg()), artifact.BuildInfo{BaseID: baseIDOf(t, dir, "codes"), Entries: 2}); err != nil {
		t.Fatal(err)
	}

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := st2.List("codes")
	if c := mustQuery(t, l, "doctored-key"); len(c) != 1 || c[0].EntryID != "c1" {
		t.Fatalf("doctored artifact not used (rebuild happened?): %+v", c)
	}
	if c := mustQuery(t, l, "aa-1"); len(c) != 0 {
		t.Fatalf("doctored artifact not used, base keys still indexed: %+v", c)
	}
}

// TestStoreExactCorruptArtifactFallsBack: a corrupt exact base.idx must not
// fail Open; results must match a clean rebuild from base.jsonl.
func TestStoreExactCorruptArtifactFallsBack(t *testing.T) {
	dir := seedExactStore(t)
	idxPath := filepath.Join(dir, "codes", "base.idx")
	if err := os.WriteFile(idxPath, []byte("KURNEXA1 this is garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("corrupt artifact failed startup: %v", err)
	}
	l, _ := st.List("codes")
	if c := mustQuery(t, l, "aa-1"); len(c) != 1 || c[0].EntryID != "c1" {
		t.Fatalf("fallback rebuild wrong: %+v", c)
	}
	if c := mustQuery(t, l, "bb-2"); len(c) != 1 || c[0].EntryID != "c2" {
		t.Fatalf("fallback rebuild wrong: %+v", c)
	}
	if e, o, tb := l.Stats(); e != 2 || o != 0 || tb != 0 {
		t.Fatalf("fallback stats %d,%d,%d", e, o, tb)
	}
}

// TestStoreExactMismatchedArtifactFallsBack: a valid exact artifact whose
// ordinals exceed base.jsonl's entry count must be rejected
// (ReplaceWithExactIndex validation) and fall back to a full build.
func TestStoreExactMismatchedArtifactFallsBack(t *testing.T) {
	dir := seedExactStore(t) // base.jsonl has 2 entries
	b := exact.NewBuilder()
	b.Add(0, []string{"aa-1"})
	b.Add(1, []string{"bb-2"})
	b.Add(2, []string{"cc-3"}) // 3 ords > 2 entries
	oversized, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if err := artifact.SaveExact(filepath.Join(dir, "codes", "base.idx"), oversized, cfgDigest(t, exactCfg()), artifact.BuildInfo{BaseID: baseIDOf(t, dir, "codes"), Entries: 3}); err != nil {
		t.Fatal(err)
	}

	st, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("mismatched artifact failed startup: %v", err)
	}
	l, _ := st.List("codes")
	idx := l.BaseExact()
	if idx == nil {
		t.Fatal("no base index after fallback")
	}
	// A rebuilt index has exactly 2 ordinals; the doctored artifact has 3.
	if n := idx.NumOrds(); n != 2 {
		t.Fatalf("mismatched artifact was installed: NumOrds %d, want 2", n)
	}
	if c := mustQuery(t, l, "cc-3"); len(c) != 0 {
		t.Fatalf("doctored key survived fallback: %+v", c)
	}
}

func TestStoreListsStableOrder(t *testing.T) {
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"zeta", "alpha", "mid"} {
		if _, err := st.CreateList(n, personCfg()); err != nil {
			t.Fatal(err)
		}
	}
	var names []string
	for _, l := range st.Lists() {
		names = append(names, l.Name())
	}
	want := []string{"alpha", "mid", "zeta"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("Lists order %v, want %v", names, want)
		}
	}
}

// TestStoreReplaceDedupesOnDisk: Replace persists the DEDUPED entries so
// base.jsonl and base.idx agree; restart via the artifact fast path works.
func TestStoreReplaceDedupesOnDisk(t *testing.T) {
	dir := t.TempDir()
	st, _ := engine.Open(dir)
	st.CreateList("people", personCfg())
	if err := st.Replace("people", []engine.Entry{
		{ID: "p1", Keys: []string{"Marcus Chen"}},
		{ID: "p1", Keys: []string{"Marcus Chao"}}, // last wins
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "people", "base.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(raw, []byte("\n")); n != 1 {
		t.Fatalf("base.jsonl has %d lines, want 1 (deduped): %s", n, raw)
	}
	if !bytes.Contains(raw, []byte("Marcus Chao")) || bytes.Contains(raw, []byte("Marcus Chen")) {
		t.Fatalf("base.jsonl kept the wrong duplicate (want last-wins): %s", raw)
	}

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := st2.List("people")
	if e, _, _ := l.Stats(); e != 1 {
		t.Fatalf("restart entries = %d, want 1", e)
	}
	// The surviving (last) duplicate's key must be what gets attributed.
	if c := mustQuery(t, l, "marcus chao"); len(c) != 1 || c[0].EntryID != "p1" || c[0].Key != "Marcus Chao" {
		t.Fatalf("deduped replace after restart: %+v", c)
	}
}

// TestStoreTornJournalTail: a truncated final journal line (crash mid-append)
// must not fail startup; intact records before it replay.
func TestStoreTornJournalTail(t *testing.T) {
	dir := t.TempDir()
	st, _ := engine.Open(dir)
	st.CreateList("people", personCfg())
	st.Upsert("people", []engine.Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}})
	jp := filepath.Join(dir, "people", "journal.jsonl")
	f, err := os.OpenFile(jp, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"op":"upsert","entry":{"id":"p2","ke`) // torn write
	f.Close()

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("torn journal failed startup: %v", err)
	}
	l, _ := st2.List("people")
	if c := mustQuery(t, l, "marcus chen"); len(c) != 1 || c[0].EntryID != "p1" {
		t.Fatalf("intact records before torn tail lost: %+v", c)
	}
}

// TestStoreCompactFreshList: Compact before any journaled mutation (journal
// file does not exist yet) must succeed — regression for ENOENT on truncate.
func TestStoreCompactFreshList(t *testing.T) {
	dir := t.TempDir()
	st, _ := engine.Open(dir)
	if _, err := st.CreateList("people", personCfg()); err != nil {
		t.Fatal(err)
	}
	if err := st.Compact("people"); err != nil {
		t.Fatalf("compact on fresh list: %v", err)
	}
	if _, err := engine.Open(dir); err != nil {
		t.Fatalf("restart after fresh compact: %v", err)
	}
}

// TestStoreTornJournalRepairedOnOpen: Open must TRUNCATE a torn journal tail,
// not just skip it — the fragment has no trailing newline, so without repair
// the next acknowledged Upsert would append onto the same line and be
// silently dropped by the following restart (losing an acknowledged write).
func TestStoreTornJournalRepairedOnOpen(t *testing.T) {
	dir := t.TempDir()
	st, _ := engine.Open(dir)
	st.CreateList("people", personCfg())
	if err := st.Upsert("people", []engine.Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}}); err != nil {
		t.Fatal(err)
	}
	jp := filepath.Join(dir, "people", "journal.jsonl")
	f, err := os.OpenFile(jp, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"op":"upsert","entry":{"id":"p2","ke`) // torn write, no newline
	f.Close()

	st2, err := engine.Open(dir) // must repair the tail
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(jp)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatalf("journal not repaired, still ends mid-record: %q", raw)
	}
	if err := st2.Upsert("people", []engine.Entry{{ID: "p3", Keys: []string{"Rosa Almeida"}}}); err != nil {
		t.Fatal(err) // acknowledged
	}

	st3, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := st3.List("people")
	if c := l.Query("rosa almeida", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "p3" {
		t.Fatalf("acknowledged post-repair upsert lost on restart: %+v", c)
	}
	if c := l.Query("marcus chen", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "p1" {
		t.Fatalf("intact pre-tear record lost: %+v", c)
	}
	if e, _, _ := l.Stats(); e != 2 {
		t.Fatalf("entries = %d, want 2 (p1, p3)", e)
	}
}

// TestStoreNewlinelessTailDropped: a final record that parses but lacks its
// trailing newline is a torn write too (append is line+'\n' in one Write, so
// it was never acknowledged) — it must be dropped and truncated away.
func TestStoreNewlinelessTailDropped(t *testing.T) {
	dir := t.TempDir()
	st, _ := engine.Open(dir)
	st.CreateList("people", personCfg())
	if err := st.Upsert("people", []engine.Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}}); err != nil {
		t.Fatal(err)
	}
	jp := filepath.Join(dir, "people", "journal.jsonl")
	f, _ := os.OpenFile(jp, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"op":"delete","id":"p1"}`) // complete JSON, torn before '\n'
	f.Close()

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := st2.List("people")
	if c := l.Query("marcus chen", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "p1" {
		t.Fatalf("unacknowledged newline-less delete was replayed: %+v", c)
	}
	raw, _ := os.ReadFile(jp)
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatalf("journal not repaired: %q", raw)
	}
}

// writeEntryLines writes entries as a base.jsonl-format file (test fixture
// helper for constructing mid-crash on-disk states).
func writeEntryLines(t *testing.T, path string, entries ...engine.Entry) {
	t.Helper()
	var buf bytes.Buffer
	for i := range entries {
		line, err := json.Marshal(&entries[i])
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestStoreOversizeEntryRejected: an entry whose persisted line would exceed
// the 1 MiB read limit must be rejected at write time with nothing persisted
// — readJournal/readBase refuse such lines, so acknowledging one would brick
// the next Open.
func TestStoreOversizeEntryRejected(t *testing.T) {
	dir := t.TempDir()
	st, _ := engine.Open(dir)
	st.CreateList("people", personCfg())
	if err := st.Replace("people", []engine.Entry{{ID: "small", Keys: []string{"Marcus Chen"}}}); err != nil {
		t.Fatal(err)
	}
	big := engine.Entry{
		ID:      "big",
		Keys:    []string{"Big Guy"},
		Payload: json.RawMessage(`{"blob":"` + strings.Repeat("x", 2<<20) + `"}`), // 2 MiB
	}

	// Upsert path: journal line bound. The error is the typed
	// EntryTooLargeError carrying the offending ID.
	err := st.Upsert("people", []engine.Entry{big})
	if err == nil || !strings.Contains(err.Error(), `"big"`) {
		t.Fatalf("oversize upsert: err = %v, want error naming entry id", err)
	}
	var tooBig *engine.EntryTooLargeError
	if !errors.As(err, &tooBig) || tooBig.ID != "big" || tooBig.Size <= 1<<20 {
		t.Fatalf("oversize upsert: err = %#v, want *engine.EntryTooLargeError{ID:\"big\"}", err)
	}
	if raw, rerr := os.ReadFile(filepath.Join(dir, "people", "journal.jsonl")); rerr == nil && len(raw) != 0 {
		t.Fatalf("oversize upsert persisted %d journal bytes", len(raw))
	}
	if lq, _ := st.List("people"); len(mustQuery(t, lq, "big guy")) != 0 {
		t.Fatal("oversize upsert applied in memory")
	}

	// Replace path: base line bound. Prior base files must be untouched.
	err = st.Replace("people", []engine.Entry{{ID: "small2", Keys: []string{"Dana Kovak"}}, big})
	if err == nil || !strings.Contains(err.Error(), `"big"`) {
		t.Fatalf("oversize replace: err = %v, want error naming entry id", err)
	}
	tooBig = nil
	if !errors.As(err, &tooBig) || tooBig.ID != "big" {
		t.Fatalf("oversize replace: err = %#v, want *engine.EntryTooLargeError{ID:\"big\"}", err)
	}
	raw, rerr := os.ReadFile(filepath.Join(dir, "people", "base.jsonl"))
	if rerr != nil || !bytes.Contains(raw, []byte(`"small"`)) || bytes.Contains(raw, []byte(`"small2"`)) {
		t.Fatalf("failed replace touched base.jsonl (err=%v): %.100s", rerr, raw)
	}
	if _, err := os.Stat(filepath.Join(dir, "people", "base.idx")); err != nil {
		t.Fatalf("failed replace removed base.idx: %v", err)
	}
	l, _ := st.List("people")
	if c := mustQuery(t, l, "marcus chen"); len(c) != 1 || c[0].EntryID != "small" {
		t.Fatalf("failed replace mutated memory: %+v", c)
	}

	// And a restart is fine: nothing unreadable was persisted.
	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("restart after oversize rejections: %v", err)
	}
	l2, _ := st2.List("people")
	if e, _, _ := l2.Stats(); e != 1 {
		t.Fatalf("restart entries = %d, want 1", e)
	}
}

// TestStoreCompactCrashFixture reconstructs the on-disk state of a compact
// that crashed between renaming the new (folded) base.jsonl and truncating
// the journal — with base.idx already removed. Recovery must full-build from
// the folded base and re-replay the journal, which is idempotent: the live
// set is unchanged.
func TestStoreCompactCrashFixture(t *testing.T) {
	dir := t.TempDir()
	st, _ := engine.Open(dir)
	st.CreateList("people", personCfg())
	p1 := engine.Entry{ID: "p1", Keys: []string{"Marcus Chen"}}
	p2 := engine.Entry{ID: "p2", Keys: []string{"Dana Kovak"}}
	if err := st.Replace("people", []engine.Entry{p1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("people", []engine.Entry{p2}); err != nil {
		t.Fatal(err)
	}
	// Doctor: folded base in place, artifact removed, journal NOT truncated.
	writeEntryLines(t, filepath.Join(dir, "people", "base.jsonl"), p1, p2)
	if err := os.Remove(filepath.Join(dir, "people", "base.idx")); err != nil {
		t.Fatal(err)
	}

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("recovery from compact crash point: %v", err)
	}
	l, _ := st2.List("people")
	entries, overlay, tombs := l.Stats()
	if entries != 2 {
		t.Fatalf("live entries = %d (overlay %d, tombs %d), want 2", entries, overlay, tombs)
	}
	if c := mustQuery(t, l, "marcus chen"); len(c) != 1 || c[0].EntryID != "p1" {
		t.Fatalf("p1 after recovery: %+v", c)
	}
	if c := mustQuery(t, l, "dana kovak"); len(c) != 1 || c[0].EntryID != "p2" {
		t.Fatalf("idempotent journal re-replay: %+v", c)
	}
}

// TestStoreReplaceCrashFixture reconstructs a replace that crashed between
// renaming the new base.jsonl and truncating the journal: new base + OLD
// journal, no artifact. The documented (and here pinned) recovery anomaly:
// pre-replace journal ops replay onto the new base. Only the never-
// acknowledged Replace is affected; the acknowledged journaled upsert
// survives — the reverse ordering (truncate first) would lose it instead.
func TestStoreReplaceCrashFixture(t *testing.T) {
	dir := t.TempDir()
	st, _ := engine.Open(dir)
	st.CreateList("people", personCfg())
	if err := st.Replace("people", []engine.Entry{{ID: "old1", Keys: []string{"Marcus Chen"}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert("people", []engine.Entry{{ID: "old2", Keys: []string{"Dana Kovak"}}}); err != nil {
		t.Fatal(err)
	}
	// Doctor: the interrupted Replace([new1]) got as far as the base rename.
	// (Name chosen gram-disjoint from "Dana Kovak": with segment-local
	// scoring, ONE shared bigram would make the overlay's known-gram
	// denominator that single gram and score old2 a spurious 100.)
	writeEntryLines(t, filepath.Join(dir, "people", "base.jsonl"), engine.Entry{ID: "new1", Keys: []string{"Iris Bell"}})
	if err := os.Remove(filepath.Join(dir, "people", "base.idx")); err != nil {
		t.Fatal(err)
	}

	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("recovery from replace crash point: %v", err)
	}
	l, _ := st2.List("people")
	if c := mustQuery(t, l, "iris bell"); len(c) != 1 || c[0].EntryID != "new1" {
		t.Fatalf("new base lost: %+v", c)
	}
	if c := mustQuery(t, l, "dana kovak"); len(c) != 1 || c[0].EntryID != "old2" {
		t.Fatalf("acknowledged journal upsert lost: %+v", c)
	}
	// old1 was only in the replaced base and not journaled: gone.
	if e, _, _ := l.Stats(); e != 2 {
		t.Fatalf("live entries = %d, want 2 (new1 + old2)", e)
	}
}

// TestStoreConcurrentStress drives the full Store surface concurrently: two
// lists mutated in parallel (upsert/delete churn on "a", churn + periodic
// Replace on "b"), a compactor looping on "a", and query workers hammering
// both lists through Store.List. Anchor entries survive every mutation, so
// each query must see an internally consistent snapshot: the anchor found
// exactly once and no duplicated EntryIDs (base+overlay leak). Run with -race.
func TestStoreConcurrentStress(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	anchorA := engine.Entry{ID: "anchor", Keys: []string{"Elena Vasquez"}}
	anchorB := engine.Entry{ID: "anchor", Keys: []string{"Omar Reyes"}}
	for name, anchor := range map[string]engine.Entry{"a": anchorA, "b": anchorB} {
		if _, err := st.CreateList(name, personCfg()); err != nil {
			t.Fatal(err)
		}
		if err := st.Replace(name, []engine.Entry{anchor}); err != nil {
			t.Fatal(err)
		}
	}

	var mut sync.WaitGroup
	churnDone := make(chan struct{})
	stop := make(chan struct{})

	mut.Add(1)
	go func() { // churn on "a"; every batch re-upserts the anchor
		defer mut.Done()
		defer close(churnDone)
		for i := 0; i < 120; i++ {
			id := fmt.Sprintf("churn%d", i%8)
			if err := st.Upsert("a", []engine.Entry{
				anchorA,
				{ID: id, Keys: []string{fmt.Sprintf("Churn Person %d", i%8)}},
			}); err != nil {
				t.Errorf("Upsert(a): %v", err)
				return
			}
			if err := st.Delete("a", id); err != nil {
				t.Errorf("Delete(a): %v", err)
				return
			}
		}
	}()
	mut.Add(1)
	go func() { // churn + periodic full Replace on "b"
		defer mut.Done()
		for i := 0; i < 120; i++ {
			id := fmt.Sprintf("bchurn%d", i%8)
			if err := st.Upsert("b", []engine.Entry{
				{ID: id, Keys: []string{fmt.Sprintf("Beta Person %d", i%8)}},
			}); err != nil {
				t.Errorf("Upsert(b): %v", err)
				return
			}
			if i%20 == 19 {
				if err := st.Replace("b", []engine.Entry{anchorB}); err != nil {
					t.Errorf("Replace(b): %v", err)
					return
				}
			}
		}
	}()
	mut.Add(1)
	go func() { // compactor races Compact("a") against the churn
		defer mut.Done()
		for {
			select {
			case <-churnDone:
				return
			default:
			}
			if err := st.Compact("a"); err != nil {
				t.Errorf("Compact(a): %v", err)
				return
			}
			runtime.Gosched()
		}
	}()
	go func() {
		mut.Wait()
		close(stop)
	}()

	queries := map[string]string{"a": "Elena Vasquez", "b": "Omar Reyes"}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		name := []string{"a", "b"}[w%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				l, ok := st.List(name)
				if !ok {
					t.Errorf("List(%s) lost", name)
					return
				}
				c := l.Query(queries[name], engine.QueryOpts{})
				seen := map[string]int{}
				for _, cand := range c {
					seen[cand.EntryID]++
				}
				for id, n := range seen {
					if n > 1 {
						t.Errorf("list %s: duplicate candidate %q (x%d): %+v", name, id, n, c)
						return
					}
				}
				if seen["anchor"] != 1 {
					t.Errorf("list %s: anchor not found exactly once: %+v", name, c)
					return
				}
			}
		}()
	}
	mut.Wait()
	wg.Wait()

	// The whole run must be replayable: restart and re-check the anchors.
	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, q := range queries {
		l, ok := st2.List(name)
		if !ok {
			t.Fatalf("list %s lost after restart", name)
		}
		found := false
		for _, cand := range l.Query(q, engine.QueryOpts{}) {
			if cand.EntryID == "anchor" {
				found = true
			}
		}
		if !found {
			t.Errorf("list %s: anchor missing after restart", name)
		}
	}
}

// A preset must be persisted RESOLVED to its explicit step list, with the
// preset name cleared: config.json is the authority on reopen, so a future
// edit to a built-in preset must never silently re-analyze an existing list
// differently.
func TestStorePresetPersistedAsSteps(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("people", personCfg()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "people", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg engine.ListConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	wantSteps, ok := analyzer.PresetSteps("person-name")
	if !ok {
		t.Fatal("person-name preset missing")
	}
	if cfg.Analyzer.Preset != "" {
		t.Errorf("persisted config carries preset name %q, want cleared", cfg.Analyzer.Preset)
	}
	if !slices.Equal(cfg.Analyzer.Steps, wantSteps) {
		t.Errorf("persisted steps = %v, want %v", cfg.Analyzer.Steps, wantSteps)
	}

	// The reopened list analyzes identically (preset behavior via steps).
	if err := st.Replace("people", []engine.Entry{{ID: "p1", Keys: []string{"Vasquez, Elena"}}}); err != nil {
		t.Fatal(err)
	}
	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, ok2 := st2.List("people")
	if !ok2 {
		t.Fatal("list lost on restart")
	}
	if got := l.Config().Analyzer; got.Preset != "" || !slices.Equal(got.Steps, wantSteps) {
		t.Errorf("reopened analyzer config = %+v, want steps %v", got, wantSteps)
	}
	// sort_tokens + strip_punctuation still apply: reordered query matches.
	if c := l.Query("elena vasquez", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "p1" {
		t.Fatalf("reopened list query: %+v", c)
	}
}
