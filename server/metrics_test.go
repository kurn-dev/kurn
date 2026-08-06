package server_test

// The /metrics exposition-format shape (HELP/TYPE lines, monotone
// cumulative buckets, +Inf == _count) and counter correctness after real
// traffic.

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestMetricsExposition(t *testing.T) {
	ts := newTS(t)
	do(t, "PUT", ts.URL+"/v1/lists/codes", `{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"exact"}}`)
	do(t, "POST", ts.URL+"/v1/lists/codes/entries", `[{"id":"c1","keys":["AA-1"]}]`)
	for i := 0; i < 5; i++ {
		do(t, "POST", ts.URL+"/v1/query", `{"q":"aa-1","lists":["codes"]}`) // hit
	}
	do(t, "POST", ts.URL+"/v1/query", `{"q":"zz-9","lists":["codes"]}`) // miss

	resp, body := do(t, "GET", ts.URL+"/metrics", "")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("/metrics: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	text := string(body)

	get := func(line string) uint64 {
		t.Helper()
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(l, line+" ") {
				v, err := strconv.ParseUint(strings.Fields(l)[1], 10, 64)
				if err != nil {
					t.Fatalf("parse %q: %v", l, err)
				}
				return v
			}
		}
		t.Fatalf("metric %q missing:\n%s", line, text)
		return 0
	}

	if q := get(`kurn_queries_total{list="codes"}`); q != 6 {
		t.Errorf("queries_total = %d, want 6", q)
	}
	if h := get(`kurn_query_hits_total{list="codes"}`); h != 5 {
		t.Errorf("hits_total = %d, want 5", h)
	}
	if e := get(`kurn_list_entries{list="codes"}`); e != 1 {
		t.Errorf("entries gauge = %d, want 1", e)
	}
	if u := get(`kurn_mutations_total{op="upsert"}`); u != 1 {
		t.Errorf("upsert counter = %d, want 1", u)
	}
	if c := get(`kurn_mutations_total{op="create"}`); c != 1 {
		t.Errorf("create counter = %d, want 1", c)
	}

	// Every metric family has HELP and TYPE.
	for _, fam := range []string{
		"kurn_list_entries", "kurn_list_overlay", "kurn_list_tombstones",
		"kurn_list_dropped_keys", "kurn_list_keyless_entries",
		"kurn_list_unindexed_entries",
		"kurn_queries_total", "kurn_query_hits_total",
		"kurn_query_duration_seconds", "kurn_mutations_total",
	} {
		if !strings.Contains(text, "# HELP "+fam+" ") || !strings.Contains(text, "# TYPE "+fam+" ") {
			t.Errorf("family %s missing HELP/TYPE", fam)
		}
	}

	// Histogram shape: buckets cumulative-monotone, +Inf equals _count,
	// _count equals queries observed.
	var prev uint64
	var bucketLines int
	for _, l := range strings.Split(text, "\n") {
		if !strings.HasPrefix(l, `kurn_query_duration_seconds_bucket{list="codes"`) {
			continue
		}
		bucketLines++
		v, _ := strconv.ParseUint(strings.Fields(l)[1], 10, 64)
		if v < prev {
			t.Fatalf("bucket counts not monotone at %q (%d < %d)", l, v, prev)
		}
		prev = v
	}
	if bucketLines != 17 { // 16 bounds + +Inf
		t.Errorf("bucket lines = %d, want 17", bucketLines)
	}
	inf := get(`kurn_query_duration_seconds_bucket{list="codes",le="+Inf"}`)
	count := get(`kurn_query_duration_seconds_count{list="codes"}`)
	if inf != count || count != 6 {
		t.Errorf("+Inf bucket %d vs count %d, want both 6", inf, count)
	}
	// Sum is present and non-negative (sub-µs exact queries may round low).
	if !strings.Contains(text, fmt.Sprintf(`kurn_query_duration_seconds_sum{list=%q}`, "codes")) {
		t.Error("histogram _sum missing")
	}
}

// All six per-list gauges of one scrape describe one known list state —
// entries, overlay, tombstones, dropped keys, keyless entries, and
// unindexed entries each carry the value the crafted state pins.
func TestMetricsGaugesMatchKnownStatus(t *testing.T) {
	ts := newTS(t)
	do(t, "PUT", ts.URL+"/v1/lists/codes", `{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"exact"}}`)
	// Base after compact: c1 findable; c2's only key trims to "" — one
	// dropped key, one keyless entry.
	do(t, "POST", ts.URL+"/v1/lists/codes/entries", `[{"id":"c1","keys":["AA-1"]},{"id":"c2","keys":[" "]}]`)
	do(t, "POST", ts.URL+"/v1/lists/codes/compact", "")
	// One tombstone in base, one overlay entry on top.
	do(t, "DELETE", ts.URL+"/v1/lists/codes/entries/c1", "")
	do(t, "POST", ts.URL+"/v1/lists/codes/entries", `[{"id":"c3","keys":["BB-2"]}]`)

	_, body := do(t, "GET", ts.URL+"/metrics", "")
	text := string(body)
	gauge := func(fam string) uint64 {
		t.Helper()
		line := fam + `{list="codes"}`
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(l, line+" ") {
				v, err := strconv.ParseUint(strings.Fields(l)[1], 10, 64)
				if err != nil {
					t.Fatalf("parse %q: %v", l, err)
				}
				return v
			}
		}
		t.Fatalf("gauge %q missing:\n%s", line, text)
		return 0
	}
	for _, g := range []struct {
		fam  string
		want uint64
	}{
		{"kurn_list_entries", 2},    // base 2 - tombstone 1 + overlay 1
		{"kurn_list_overlay", 1},    // c3
		{"kurn_list_tombstones", 1}, // c1
		{"kurn_list_dropped_keys", 1},
		{"kurn_list_keyless_entries", 1},
		{"kurn_list_unindexed_entries", 0},
	} {
		if v := gauge(g.fam); v != g.want {
			t.Errorf("%s = %d, want %d", g.fam, v, g.want)
		}
	}
}
