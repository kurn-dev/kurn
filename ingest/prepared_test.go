package ingest_test

// Prepared artifact build — same parse and duplicate policy as the bundle
// build, and the same logical base identity.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/prepared"
	"github.com/kurn-dev/kurn/engine/prepared/builder"
	"github.com/kurn-dev/kurn/ingest"
)

// artFeed carries the characters encoding/json escapes (<, >, &, U+2028,
// U+2029), quotes, backslashes and non-ASCII in IDs, keys and payloads, plus
// one byte-identical duplicate record, which both builds collapse.
func artFeed() string {
	var b strings.Builder
	rows := []map[string]any{
		{"id": "a<1>", "name": "Anna Smith", "alias": "A. Smith", "program": "SDGT", "country": "US", "note": "<b>&amp;</b>"},
		{"id": "b&2", "name": "José Müller", "program": "SDN", "country": "DE", "note": "line\u2028sep\u2029para"},
		{"id": "c\"3", "name": "O'Brien \"Quoted\"", "alias": "Back\\slash", "program": "SDGT", "country": "IR"},
		{"id": "d\u20284", "name": "Øster Şahin", "program": "IRAN", "country": "IR", "note": "ünïcödé"},
		{"id": "e5", "name": "Acme <Holdings> & Co", "program": "SDN", "country": "RU"},
	}
	for i := 0; i < 60; i++ {
		rows = append(rows, map[string]any{"id": fmt.Sprintf("gen-%03d", i), "name": fmt.Sprintf("Person %d Nakamura", i), "program": []string{"SDGT", "SDN"}[i%2], "country": []string{"US", "DE", "IR"}[i%3]})
	}
	for i, r := range rows {
		line, _ := json.Marshal(r)
		b.Write(line)
		b.WriteByte('\n')
		if i == 1 {
			b.Write(line) // byte-identical duplicate: collapsed, counted
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func artMapping(mode string) *ingest.Mapping {
	m := &ingest.Mapping{
		Format:  "ndjson",
		ID:      "id",
		Keys:    []ingest.KeyRule{{Path: "name"}, {Path: "alias"}},
		Payload: map[string]string{"program": "program", "country": "country", "note": "note"},
	}
	switch mode {
	case "exact":
		m.List = engine.ListConfig{Analyzer: engine.AnalyzerConfig{Preset: "identifier"}, Match: engine.MatchConfig{Mode: "exact"}}
	case "ngram":
		m.List = engine.ListConfig{Analyzer: engine.AnalyzerConfig{Preset: "person-name"}, Match: engine.MatchConfig{Mode: "ngram"}}
	case "filtered":
		m.List = engine.ListConfig{Analyzer: engine.AnalyzerConfig{Preset: "person-name"}, Match: engine.MatchConfig{Mode: "ngram", Grams: []int{3}},
			Filterable: []engine.FilterField{{Name: "program", Path: "program"}, {Name: "country", Path: "country"}}}
	}
	return m
}

func TestBuildArtifactMatchesBundle(t *testing.T) {
	feed := artFeed()
	feedSum := sha256.Sum256([]byte(feed))
	for _, mode := range []string{"exact", "ngram", "filtered"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			man, err := ingest.Build(artMapping(mode), strings.NewReader(feed), filepath.Join(dir, "bundle"), ingest.BuildOptions{})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "out", "list.kurn")
			res, err := ingest.BuildArtifact(artMapping(mode), strings.NewReader(feed), path, ingest.ArtifactOptions{Name: "list"})
			if err != nil {
				t.Fatal(err)
			}
			base, err := os.ReadFile(filepath.Join(dir, "bundle", "base.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			baseSum := sha256.Sum256(base)
			if res.BaseID != man.SHA256 || res.BaseID != hex.EncodeToString(baseSum[:]) {
				t.Fatalf("artifact base %s, bundle manifest %s, base.jsonl %x", res.BaseID, man.SHA256, baseSum)
			}
			if res.Entries != man.Entries || res.Keys != man.Keys || res.Collapsed != man.Collapsed || res.Collapsed != 1 || res.Mode != man.Mode || res.ConfigSHA256 != man.ConfigSHA256 {
				t.Fatalf("artifact %+v disagrees with manifest %+v", res, man)
			}
			if res.InputSHA256 != hex.EncodeToString(feedSum[:]) {
				t.Fatalf("input sha256 %s, want %x", res.InputSHA256, feedSum)
			}
			ver, err := prepared.Verify(path, prepared.LevelFull)
			if err != nil {
				t.Fatal(err)
			}
			if ver.BaseID != res.BaseID || ver.ContainerSHA256 != res.ContainerSHA256 || ver.Name != "list" {
				t.Fatalf("verified %+v, result %+v", ver, res)
			}
			fi, _ := os.Stat(path)
			if fi.Mode().Perm() != 0o444 {
				t.Fatalf("mode %v", fi.Mode().Perm())
			}
			// Record order and bytes are the bundle's base.jsonl, line by line.
			buf, _ := prepared.ReadFile(path)
			v, err := prepared.NewView(buf)
			if err != nil {
				t.Fatal(err)
			}
			lines := bytes.Split(bytes.TrimSuffix(base, []byte("\n")), []byte("\n"))
			if len(lines) != v.Len() {
				t.Fatalf("%d records, %d base lines", v.Len(), len(lines))
			}
			for i, line := range lines {
				var e engine.Entry
				if err := json.Unmarshal(line, &e); err != nil {
					t.Fatal(err)
				}
				if e.ID != v.ID(i) || !bytes.Equal(e.Payload, v.Payload(i)) || strings.Join(e.Keys, "\x00") != strings.Join(v.Keys(i), "\x00") {
					t.Fatalf("record %d differs from base line %s", i, line)
				}
			}
			if b := v.Build(); b.InputSHA256 != res.InputSHA256 || b.Collapsed != 1 {
				t.Fatalf("build provenance %+v", b)
			}
		})
	}
}

func TestBuildArtifactMatchesBuildEntries(t *testing.T) {
	var entries []engine.Entry
	for i := 0; i < 40; i++ {
		e := engine.Entry{ID: fmt.Sprintf("p%02d<&>", i), Keys: []string{fmt.Sprintf("Name %d", i), "Ünï\u2028"}}
		if i%2 == 0 {
			e.Payload = json.RawMessage(fmt.Sprintf(`{ "k" : "<%d>&`+"\u2029"+`" }`, i))
		}
		entries = append(entries, e)
	}
	cfg := artMapping("exact").List
	dir := t.TempDir()
	man, err := ingest.BuildEntries(cfg, append([]engine.Entry(nil), entries...), filepath.Join(dir, "b"), ingest.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := builder.Write(filepath.Join(dir, "a.kurn"), "a", cfg, entries, builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.BaseID != man.SHA256 || res.ConfigDigest != man.ConfigSHA256 {
		t.Fatalf("artifact %s/%s, bundle %s/%s", res.BaseID, res.ConfigDigest, man.SHA256, man.ConfigSHA256)
	}
}

func TestBuildArtifactDuplicatePolicyMatchesBuild(t *testing.T) {
	feed := `{"id":"a","name":"Anna"}
{"id":"a","name":"Other Anna"}
{"id":"b","name":"Bo"}
`
	dir := t.TempDir()
	_, bundleErr := ingest.Build(artMapping("exact"), strings.NewReader(feed), filepath.Join(dir, "b"), ingest.BuildOptions{})
	path := filepath.Join(dir, "a.kurn")
	_, artErr := ingest.BuildArtifact(artMapping("exact"), strings.NewReader(feed), path, ingest.ArtifactOptions{Name: "a"})
	if bundleErr == nil || artErr == nil || bundleErr.Error() != artErr.Error() {
		t.Fatalf("bundle error %v, artifact error %v", bundleErr, artErr)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("a failed build left an artifact")
	}
	_, bundleErr = ingest.Build(artMapping("exact"), strings.NewReader(""), filepath.Join(dir, "c"), ingest.BuildOptions{})
	_, artErr = ingest.BuildArtifact(artMapping("exact"), strings.NewReader(""), path, ingest.ArtifactOptions{Name: "a"})
	if bundleErr == nil || artErr == nil || bundleErr.Error() != artErr.Error() {
		t.Fatalf("empty feed: bundle error %v, artifact error %v", bundleErr, artErr)
	}
	// Bad records: same skip-bad accounting.
	bad := "{\"id\":\"a\",\"name\":\"Anna\"}\n{\"id\":\"\",\"name\":\"x\"}\n"
	_, bundleErr = ingest.Build(artMapping("exact"), strings.NewReader(bad), filepath.Join(dir, "d"), ingest.BuildOptions{})
	_, artErr = ingest.BuildArtifact(artMapping("exact"), strings.NewReader(bad), path, ingest.ArtifactOptions{Name: "a"})
	if bundleErr == nil || artErr == nil || bundleErr.Error() != artErr.Error() {
		t.Fatalf("bad record: bundle error %v, artifact error %v", bundleErr, artErr)
	}
	res, err := ingest.BuildArtifact(artMapping("exact"), strings.NewReader(bad), path, ingest.ArtifactOptions{Name: "a", SkipBad: 1})
	if err != nil || res.Entries != 1 {
		t.Fatalf("skip-bad: %+v, %v", res, err)
	}
}

func TestBuildArtifactRefusesExistingAndBadNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "list.kurn")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ingest.BuildArtifact(artMapping("exact"), strings.NewReader(artFeed()), path, ingest.ArtifactOptions{Name: "list"})
	if !errors.Is(err, prepared.ErrExists) {
		t.Fatalf("got %v, want ErrExists", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "keep me" {
		t.Fatal("existing file modified")
	}
	for _, name := range []string{"", "Upper", "a b", "../x", strings.Repeat("x", 65)} {
		r := strings.NewReader(artFeed())
		if _, err := ingest.BuildArtifact(artMapping("exact"), r, filepath.Join(dir, "n.kurn"), ingest.ArtifactOptions{Name: name}); err == nil {
			t.Fatalf("name %q accepted", name)
		}
		if r.Len() != len(artFeed()) {
			t.Fatalf("name %q: the feed was read before the name was checked", name)
		}
	}
}

func csvMapping() *ingest.Mapping {
	return &ingest.Mapping{Format: "csv", ID: "id", Keys: []ingest.KeyRule{{Path: "name"}}, List: artMapping("exact").List}
}

// dirNames lists a directory's entries.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// TestBuildArtifactRefusesNonUTF8 (F1, F2): an ID or key that is not valid
// UTF-8 is refused before the duplicate comparison — whose json.Marshal
// would coerce it to U+FFFD and could discard it as a "byte-identical"
// duplicate — so the outcome never depends on row order, and skip-bad
// never applies. Every refusal leaves the directory empty: no artifact and
// no temporary file.
func TestBuildArtifactRefusesNonUTF8(t *testing.T) {
	for name, tc := range map[string]struct {
		feed    string
		skipBad int
		want    []string
	}{
		"single invalid key":            {feed: "id,name\n1,Anna\n2,Bad\xffName\n", want: []string{"entry 2", `(ID "2")`, "key 0"}},
		"(a) valid then invalid":        {feed: "id,name\n1,Bad\uFFFDName\n1,Bad\xffName\n", want: []string{"entry 2", `(ID "1")`, "key 0"}},
		"(b) invalid then valid":        {feed: "id,name\n1,Bad\xffName\n1,Bad\uFFFDName\n", want: []string{"entry 1", `(ID "1")`, "key 0"}},
		"(c) two distinct invalid":      {feed: "id,name\n1,Bad\xffName\n1,Bad\xfeName\n", want: []string{"entry 1", "key 0"}},
		"(e) invalid ID after valid ID": {feed: "id,name\nX\uFFFD,Anna\nX\xff,Anna\n", want: []string{"entry 2", `ID "X\xff"`}},
		"(e) invalid ID first":          {feed: "id,name\nX\xff,Anna\nX\uFFFD,Anna\n", want: []string{"entry 1", `ID "X\xff"`}},
		"(f) skip-bad does not apply":   {feed: "id,name\n1,Anna\n2,Bad\xffName\n3,Cid\n", skipBad: 5, want: []string{"entry 2", "-skip-bad does not apply to invalid UTF-8"}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "a.kurn")
			_, err := ingest.BuildArtifact(csvMapping(), strings.NewReader(tc.feed), path, ingest.ArtifactOptions{Name: "a", SkipBad: tc.skipBad})
			if err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
				t.Fatalf("got %v, want a UTF-8 refusal", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("error %q does not contain %q", err, w)
				}
			}
			// (g) nothing published, no temporary left.
			if names := dirNames(t, dir); len(names) != 0 {
				t.Fatalf("a refused build left %v", names)
			}
		})
	}
}

// Legacy duplicate detection compares encoding/json bytes, not decoded string
// equality. Go versions can encode a literal U+FFFD and a substituted invalid
// byte differently. Check the existing byte rule without making the artifact's
// strict UTF-8 refusal depend on that representation choice.
func TestLegacyInvalidUTF8DuplicateUsesJSONBytes(t *testing.T) {
	const feed = "id,name\n1,Bad\uFFFDName\n1,Bad\xffName\n"
	var encoded [][]byte
	_, err := ingest.Parse(csvMapping(), strings.NewReader(feed), ingest.Options{}, func(e engine.Entry) error {
		raw, err := json.Marshal(e)
		encoded = append(encoded, raw)
		return err
	})
	if err != nil || len(encoded) != 2 {
		t.Fatalf("parse: %v, rows %d", err, len(encoded))
	}
	man, err := ingest.Build(csvMapping(), strings.NewReader(feed), filepath.Join(t.TempDir(), "b"), ingest.BuildOptions{})
	if bytes.Equal(encoded[0], encoded[1]) {
		if err != nil || man.Entries != 1 || man.Collapsed != 1 {
			t.Fatalf("identical JSON bytes must collapse: %+v, %v", man, err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "duplicate record IDs") {
		t.Fatalf("different JSON bytes must conflict: %+v, %v", man, err)
	}
}

// TestBuildArtifactStillCollapsesValidDuplicates is case (d): a genuinely
// byte-identical valid duplicate (non-ASCII included) still collapses.
func TestBuildArtifactStillCollapsesValidDuplicates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.kurn")
	res, err := ingest.BuildArtifact(csvMapping(), strings.NewReader("id,name\n1,Bad\uFFFDName\n2,Zoë\n1,Bad\uFFFDName\n"), path, ingest.ArtifactOptions{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Entries != 2 || res.Keys != 2 || res.Collapsed != 1 {
		t.Fatalf("result %+v, want 2 entries, 2 keys, 1 collapsed", res)
	}
	if names := dirNames(t, dir); len(names) != 1 || names[0] != "a.kurn" {
		t.Fatalf("directory holds %v", names)
	}
}
