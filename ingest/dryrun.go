// Dry-run gate: run the first N records through the FULL
// pipeline — mapping, then the list's analyzer — and report what a build
// would produce, without building. The headline number is the empty-key-
// after-analysis rate: keys the analyzer collapses to "" are silently
// unfindable at query time ("half my list vanished"), so the gate makes
// the loss visible BEFORE the 10 GB build, and a threshold makes it
// CI-enforceable.
package ingest

import (
	"fmt"
	"io"

	"github.com/kurn-dev/kurn/engine"
)

// DryRunReport is what the gate saw in the sampled prefix.
type DryRunReport struct {
	// Sampled is how many records were examined (yielded + filtered + bad).
	Sampled  int `json:"sampled"`
	Records  int `json:"records"`  // records that yielded an entry
	Filtered int `json:"filtered"` // excluded by the mapping's where
	Bad      int `json:"bad"`      // bad records tolerated by skip-bad

	DistinctIDs  int `json:"distinct_ids"`
	DuplicateIDs int `json:"duplicate_ids"` // within the sample

	RawKeys        int `json:"raw_keys"`
	AnalyzedKeys   int `json:"analyzed_keys"`   // keys surviving analysis
	EmptyKeys      int `json:"empty_keys"`      // keys the analyzer collapsed to ""
	KeylessEntries int `json:"keyless_entries"` // entries with NO surviving key — unfindable

	// EmptyKeyRate = EmptyKeys / RawKeys (0 when no keys).
	EmptyKeyRate float64 `json:"empty_key_rate"`

	// EstMemoryBytes is a rough serving-memory estimate for the records
	// SAMPLED, not for the whole feed: AnalyzedKeys × the measured B/key
	// of the list's mode (bench records: ~114 ngram, ~133 exact). A
	// dry-run stops at its sample size and never learns the feed's total
	// record count, so it cannot scale this itself — multiply by
	// (total records / Sampled) to project. An estimate either way, not a
	// promise: corpus shape moves it.
	EstMemoryBytes int64 `json:"est_memory_bytes"`

	// Samples are the first few mapped entries with their analyzed keys —
	// eyeball material.
	Samples []DrySample `json:"samples"`
}

// DrySample is one mapped entry as the index would see it.
type DrySample struct {
	ID           string   `json:"id"`
	Keys         []string `json:"keys"`
	AnalyzedKeys []string `json:"analyzed_keys"`
	Payload      string   `json:"payload,omitempty"`
}

// bPerKey are the measured end-to-end memory costs from the bench record
// runs (see bench/README.md) — heuristics for the estimate only.
func bPerKey(mode string) float64 {
	if mode == "exact" {
		return 133.0
	}
	return 114.0
}

// DryRun parses up to sampleN records (yielded entries) through mapping +
// analyzer and reports. maxSamples bounds the eyeball section.
func DryRun(m *Mapping, r io.Reader, opts Options, sampleN, maxSamples int) (*DryRunReport, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	an, err := engine.ResolveAnalyzer(m.List.Analyzer)
	if err != nil {
		return nil, err
	}
	rep := &DryRunReport{}
	seen := map[string]struct{}{}
	var errStop = fmt.Errorf("dry-run sample complete")
	st, err := Parse(m, r, opts, func(e engine.Entry) error {
		rep.Records++
		if _, dup := seen[e.ID]; dup {
			rep.DuplicateIDs++
		} else {
			seen[e.ID] = struct{}{}
		}
		var analyzed []string
		for _, k := range e.Keys {
			rep.RawKeys++
			if a := an.Normalize(k); a != "" {
				rep.AnalyzedKeys++
				analyzed = append(analyzed, a)
			} else {
				rep.EmptyKeys++
			}
		}
		if len(analyzed) == 0 {
			rep.KeylessEntries++
		}
		if len(rep.Samples) < maxSamples {
			rep.Samples = append(rep.Samples, DrySample{
				ID: e.ID, Keys: e.Keys, AnalyzedKeys: analyzed, Payload: string(e.Payload),
			})
		}
		if rep.Records >= sampleN {
			return errStop
		}
		return nil
	})
	if err != nil && err != errStop {
		return nil, err
	}
	rep.Filtered, rep.Bad = st.Filtered, st.Bad
	rep.Sampled = rep.Records + rep.Filtered + rep.Bad
	rep.DistinctIDs = len(seen)
	if rep.RawKeys > 0 {
		rep.EmptyKeyRate = float64(rep.EmptyKeys) / float64(rep.RawKeys)
	}
	rep.EstMemoryBytes = int64(float64(rep.AnalyzedKeys) * bPerKey(m.List.Match.Mode))
	return rep, nil
}
