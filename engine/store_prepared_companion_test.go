package engine_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/prepared"
	"github.com/kurn-dev/kurn/engine/prepared/builder"
)

func TestOpenPreparedExactCompanion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parts.kurn")
	res, err := builder.Write(path, "parts", companionCfg(true), companionEntries(), builder.Options{})
	if err != nil || !res.ExactCompanion {
		t.Fatalf("write: %+v %v", res, err)
	}
	ref := companionList(t, true)
	for _, opts := range []engine.PreparedOptions{{}, {Verify: prepared.LevelTrusted}} {
		st, err := engine.OpenPrepared(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		info := st.Prepared()
		if info == nil || !info.ExactCompanion {
			t.Fatalf("prepared info: %+v", info)
		}
		l, ok := st.List("parts")
		if !ok {
			t.Fatal("list missing")
		}
		if l.BaseExactCompanion() != nil {
			t.Fatal("a read-only list handed out its companion")
		}
		for _, terms := range [][]string{{"acme-042"}, {"ACME-402", "zz-9"}, {"none"}} {
			if a, b := candIDs(anyOf(t, l, engine.QueryOpts{}, terms...)), candIDs(anyOf(t, ref, engine.QueryOpts{}, terms...)); a != b {
				t.Fatalf("%v: prepared %q, mutable %q", terms, a, b)
			}
		}
		filter, _ := engine.ParseTypedFilter([]byte(`{"tenant":"a"}`))
		p, err := l.PrepareTypedFilteredExactAnyOf([]string{"acme-042", "acme-402"}, engine.QueryOpts{}, filter)
		if err != nil {
			t.Fatal(err)
		}
		if c, _, err := p.Execute(context.Background()); err != nil || candIDs(c) != "p1,p4" {
			t.Fatalf("filtered on prepared: %+v %v", c, err)
		}
		if c := l.Query("acme 042", engine.QueryOpts{}); len(c) == 0 {
			t.Fatal("ngram query on the prepared list")
		}
		st.Close()
	}
}
