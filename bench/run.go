package bench

import (
	"sort"
	"time"

	"github.com/kurn-dev/kurn/engine"
)

// CatStats is per-category right-entity recall.
type CatStats struct {
	Cases  int
	Found  int
	Recall float64
}

// Report is one benchmark run's results.
type Report struct {
	Cases      int
	Found      int
	Recall     float64 // right-entity: truth ID among returned candidates
	ByCategory map[string]*CatStats
	P50Us      int64
	P99Us      int64
	QPS        float64
}

// Run executes every case single-threaded and verifies the RIGHT entity.
func Run(l *engine.List, corpus []Case, opts engine.QueryOpts) Report {
	rep := Report{ByCategory: map[string]*CatStats{}}
	if len(corpus) == 0 {
		// Guard: the percentile indexing below panics on an empty latency
		// slice, and 0/0 recall would be NaN.
		return rep
	}
	lat := make([]int64, 0, len(corpus))
	start := time.Now()
	for _, c := range corpus {
		q0 := time.Now()
		cands := l.Query(c.Query, opts)
		lat = append(lat, time.Since(q0).Microseconds())
		cs := rep.ByCategory[c.Category]
		if cs == nil {
			cs = &CatStats{}
			rep.ByCategory[c.Category] = cs
		}
		cs.Cases++
		rep.Cases++
		for _, cand := range cands {
			if cand.EntryID == c.TruthID {
				cs.Found++
				rep.Found++
				break
			}
		}
	}
	total := time.Since(start)
	for _, cs := range rep.ByCategory {
		cs.Recall = float64(cs.Found) / float64(cs.Cases)
	}
	rep.Recall = float64(rep.Found) / float64(rep.Cases)
	sort.Slice(lat, func(a, b int) bool { return lat[a] < lat[b] })
	rep.P50Us = lat[len(lat)/2]
	// Clamp is defensive: floor(0.99·len) is provably <= len-1 for len >= 1,
	// but tiny corpora sit right at the boundary (len=1 → index 0) — keep the
	// bound explicit so a future percentile tweak can't index past the end.
	rep.P99Us = lat[min(len(lat)-1, len(lat)*99/100)]
	rep.QPS = float64(len(corpus)) / total.Seconds()
	return rep
}
