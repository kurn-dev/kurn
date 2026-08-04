package engine_test

import (
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func domainList(t *testing.T, entries ...engine.Entry) *engine.List {
	t.Helper()
	l, err := engine.NewList("domains", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "domain"},
		Match:    engine.MatchConfig{Mode: "exact", Fallback: "parent_domain"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace(entries); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestParentDomainFallback(t *testing.T) {
	l := domainList(t,
		engine.Entry{ID: "d1", Keys: []string{"tempmail.com"}},
		engine.Entry{ID: "d2", Keys: []string{"mail.tempmail.com"}},
	)
	cases := []struct {
		q         string
		wantID    string
		wantScore float64
		wantKey   string
	}{
		{"tempmail.com", "d1", 100, "tempmail.com"},               // exact
		{"MAIL.tempmail.com.", "d2", 100, "mail.tempmail.com"},    // exact after analysis; d2 shadows d1
		{"smtp.mail.tempmail.com", "d2", 90, "mail.tempmail.com"}, // one level up
		{"a.b.c.tempmail.com", "d1", 90, "tempmail.com"},          // multi-level descent
	}
	for _, c := range cases {
		got := l.Query(c.q, engine.QueryOpts{})
		if len(got) != 1 || got[0].EntryID != c.wantID || got[0].Score != c.wantScore || got[0].Key != c.wantKey {
			t.Errorf("Query(%q) = %+v, want %s score %v key %q", c.q, got, c.wantID, c.wantScore, c.wantKey)
		}
	}
	if got := l.Query("mail.gmail.com", engine.QueryOpts{}); got != nil {
		t.Errorf("unlisted: %+v", got) // gmail.com not listed; "com" never probed
	}
}

func TestFallbackStopsAtFirstHitLevel(t *testing.T) {
	// Both the sub and the parent are listed: a query matching the sub exactly
	// must return ONLY the sub (descent stops), not merge in the parent.
	l := domainList(t,
		engine.Entry{ID: "parent", Keys: []string{"tempmail.com"}},
		engine.Entry{ID: "sub", Keys: []string{"mail.tempmail.com"}},
	)
	got := l.Query("mail.tempmail.com", engine.QueryOpts{})
	if len(got) != 1 || got[0].EntryID != "sub" || got[0].Score != 100 {
		t.Errorf("want only sub at 100, got %+v", got)
	}
}

func TestFallbackThroughTombstone(t *testing.T) {
	// Tombstoning the exact match must let descent continue to the parent.
	l := domainList(t,
		engine.Entry{ID: "parent", Keys: []string{"tempmail.com"}},
		engine.Entry{ID: "sub", Keys: []string{"mail.tempmail.com"}},
	)
	l.Delete("sub")
	got := l.Query("mail.tempmail.com", engine.QueryOpts{})
	if len(got) != 1 || got[0].EntryID != "parent" || got[0].Score != 90 {
		t.Errorf("want parent at 90 after sub tombstoned, got %+v", got)
	}
}

func TestFallbackThreshold(t *testing.T) {
	// Threshold applies in exact mode too: a per-query threshold > 0.9 puts
	// the parent-level score (90) below the bar, suppressing fallback matches
	// while exact hits (100) still pass. Default threshold changes nothing.
	l := domainList(t, engine.Entry{ID: "d1", Keys: []string{"tempmail.com"}})
	if got := l.Query("smtp.tempmail.com", engine.QueryOpts{Threshold: 0.95}); got != nil {
		t.Errorf("thr 0.95, parent-only match: %+v, want nil", got)
	}
	got := l.Query("tempmail.com", engine.QueryOpts{Threshold: 0.95})
	if len(got) != 1 || got[0].Score != 100 {
		t.Errorf("thr 0.95, exact match: %+v, want d1 at 100", got)
	}
	got = l.Query("smtp.tempmail.com", engine.QueryOpts{})
	if len(got) != 1 || got[0].Score != 90 {
		t.Errorf("default thr, parent match: %+v, want d1 at 90", got)
	}
}

func TestFallbackOverlayHit(t *testing.T) {
	// A parent domain added via Upsert lives in the OVERLAY segment: fallback
	// descent must find it exactly like a base entry.
	l := domainList(t, engine.Entry{ID: "d1", Keys: []string{"other.com"}})
	l.Upsert([]engine.Entry{{ID: "ov", Keys: []string{"tempmail.com"}}})
	got := l.Query("smtp.tempmail.com", engine.QueryOpts{})
	if len(got) != 1 || got[0].EntryID != "ov" || got[0].Score != 90 || got[0].Key != "tempmail.com" {
		t.Errorf("overlay parent hit: %+v, want ov at 90 key tempmail.com", got)
	}
}

func TestFallbackHostileShapes(t *testing.T) {
	l := domainList(t, engine.Entry{ID: "d1", Keys: []string{"tempmail.com"}})
	// Degenerate analyzed shapes must neither crash nor conjure hits — in
	// particular never via a bare-TLD probe ("..com" analyzes to "com").
	for _, q := range []string{"a..b.com", "..com", "com", ".", "...", "tempmail"} {
		if got := l.Query(q, engine.QueryOpts{}); got != nil {
			t.Errorf("Query(%q) = %+v, want nil", q, got)
		}
	}
	// A leading-dot spelling of a listed domain is the SAME domain: the
	// analyzer strips the empty leading label, so it hits at the exact level
	// (100), not as a phantom subdomain one level up (90).
	got := l.Query(".tempmail.com", engine.QueryOpts{})
	if len(got) != 1 || got[0].EntryID != "d1" || got[0].Score != 100 || got[0].Key != "tempmail.com" {
		t.Errorf("leading dot: %+v, want d1 exact at 100", got)
	}
}

func TestFallbackValidation(t *testing.T) {
	bad := []engine.MatchConfig{
		{Mode: "ngram", Fallback: "parent_domain"}, // fallback needs exact
		{Mode: "exact", Fallback: "bogus"},         // unknown fallback
	}
	for _, m := range bad {
		if _, err := engine.NewList("x", engine.ListConfig{
			Analyzer: engine.AnalyzerConfig{Preset: "identifier"}, Match: m,
		}); err == nil {
			t.Errorf("NewList(%+v): want error", m)
		}
	}
}
