package engine

import (
	"sort"
	"strings"
	"sync"

	"github.com/kurn-dev/kurn/engine/ngram"
)

// candMeta carries each candidate's origin so attribution can index the
// entry directly instead of re-hashing its (potentially long, e.g. LEIE
// composite) EntryID through byID.
type candMeta struct {
	seg *segment
	ord uint32
}

// candSort orders candidates and their metadata in lockstep: score desc,
// EntryID asc — the same stable order the pre-meta sortCandidates produced.
type candSort struct {
	c []Candidate
	m []candMeta
}

func (cs candSort) Len() int { return len(cs.c) }
func (cs candSort) Less(a, b int) bool {
	if cs.c[a].Score != cs.c[b].Score {
		return cs.c[a].Score > cs.c[b].Score
	}
	return cs.c[a].EntryID < cs.c[b].EntryID
}
func (cs candSort) Swap(a, b int) {
	cs.c[a], cs.c[b] = cs.c[b], cs.c[a]
	cs.m[a], cs.m[b] = cs.m[b], cs.m[a]
}

func sortCandidates(c []Candidate, m []candMeta) { sort.Stable(candSort{c, m}) }

// attrScratch pools attribution's per-query buffers; content-agnostic, so
// one process-wide pool serves every list.
type attrScratch struct {
	qGrams  map[string]struct{}
	keySeen map[string]struct{}
	offs    []int
}

var attrPool = sync.Pool{New: func() any {
	return &attrScratch{qGrams: map[string]struct{}{}, keySeen: map[string]struct{}{}}
}}

// attributeKeys sets Candidate.Key to the entry key whose gram set best covers
// the analyzed query. Post-rank, top-K only — but at topK 100 that is still
// ~topK×keys gram derivations per query, so the key side streams grams as
// zero-allocation substrings (ngram.EachGram) counted against the query's
// gram set: |q ∩ k| / |q|, identical to the map-intersection it replaced (set
// intersection is symmetric; the denominator stays |q|).
// matchedKey is the analyzed key that actually hit the exact index: the
// analyzed query itself, or — under parent_domain fallback — the parent
// suffix level that matched. The exact arm attributes by it so Candidate.Key
// names the LISTED domain, not the queried subdomain. Ngram callers pass the
// analyzed query for both.
func (l *List) attributeKeys(s *snapshot, analyzedQuery, matchedKey string, cands []Candidate, meta []candMeta) {
	sizes := l.cfg.Match.Grams
	as := attrPool.Get().(*attrScratch)
	defer func() {
		clear(as.qGrams)
		clear(as.keySeen)
		attrPool.Put(as)
	}()

	// query gram set, once per query — EachGram's seen map IS the set
	qs := analyzedQuery
	if l.cfg.Match.StripSpaces {
		qs = strings.ReplaceAll(qs, " ", "")
	}
	as.offs = ngram.EachGram(qs, sizes, as.qGrams, as.offs, func(string) {})
	qGrams := as.qGrams

	hits := 0
	count := func(g string) {
		if _, ok := qGrams[g]; ok {
			hits++
		}
	}

	for i := range cands {
		e := meta[i].seg.entries[meta[i].ord]
		// A base candidate whose ID was superseded in the overlay attributes
		// by the OVERLAY entry — the keys a reader of the list would see
		// (pinned by TestAttributionThroughOverlay).
		if s.overlay != nil && meta[i].seg != s.overlay {
			if ord, ok := s.overlay.byID[e.ID]; ok {
				e = s.overlay.entries[ord]
			}
		}
		if len(e.Keys) == 0 {
			continue
		}
		if len(e.Keys) == 1 {
			// argmax over one element is that element — skip normalization
			// and the gram walk entirely (the common single-key-entry case)
			cands[i].Key = e.Keys[0]
			continue
		}
		best, bestScore := "", -1.0
		for _, k := range e.Keys {
			ak := l.an.Normalize(k)
			var sc float64
			if l.cfg.Match.Mode == "exact" {
				if ak == matchedKey {
					sc = 1
				}
			} else if len(qGrams) > 0 {
				ks := ak
				if l.cfg.Match.StripSpaces {
					ks = strings.ReplaceAll(ks, " ", "")
				}
				clear(as.keySeen)
				hits = 0
				as.offs = ngram.EachGram(ks, sizes, as.keySeen, as.offs, count)
				sc = float64(hits) / float64(len(qGrams))
			}
			if sc > bestScore {
				best, bestScore = k, sc
			}
		}
		cands[i].Key = best
	}
}
