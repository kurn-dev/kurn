// Package engine is the kurn lookup engine: domain-blind lists of entries
// with fast fuzzy (char-ngram) or exact matching.
package engine

import (
	"encoding/json"
	"slices"
)

// Entry is one list record: caller-assigned ID, one or more searchable keys
// (name + aliases, variants…), and an opaque payload returned verbatim.
type Entry struct {
	ID      string          `json:"id"`
	Keys    []string        `json:"keys"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Candidate is a potential match.
type Candidate struct {
	EntryID string          `json:"entry_id"`
	Score   float64         `json:"score"` // 0..100
	Key     string          `json:"key"`   // best-matching key (post-rank attribution)
	Payload json.RawMessage `json:"payload,omitempty"`
}

// MatchConfig configures matching. Mode/Grams/StripSpaces are index-time;
// Threshold/TopK are query-time defaults, overridable per query.
//
// Default asymmetry (deliberate): NewList fills Threshold/TopK to 0.6/100 in
// ngram mode only. Exact-mode lists keep 0/0 — return every match level,
// unlimited — because exact hits are cheap, score-uniform (100, or 90 via
// parent fallback), and a silent default cap would hide hits the caller has
// no way to know exist. The HTTP layer bounds exact collection at its own
// global top-K instead.
type MatchConfig struct {
	Mode        string  `json:"mode"`               // "ngram" | "exact"
	Fallback    string  `json:"fallback,omitempty"` // "" | "parent_domain" (exact mode only): probe parent suffixes when the full key misses
	Grams       []int   `json:"grams,omitempty"`
	StripSpaces bool    `json:"strip_spaces,omitempty"`
	Threshold   float64 `json:"threshold,omitempty"`
	TopK        int     `json:"topk,omitempty"`
}

// AnalyzerConfig is either a preset name or an explicit step list.
type AnalyzerConfig struct {
	Preset string   `json:"preset,omitempty"`
	Steps  []string `json:"steps,omitempty"`
}

// GoldenProbe is one readiness assertion for a list: a fixed
// query whose outcome is known. A serving node runs every configured probe
// through the NORMAL query path at readiness checks — passing goldens prove
// the list's data and matching actually work, not merely that the process
// is up. Exactly one of ExpectID/Absent must be set.
type GoldenProbe struct {
	Q        string  `json:"q"`                   // the probe query (non-empty)
	ExpectID string  `json:"expect_id,omitempty"` // this entry must be among the candidates
	Absent   bool    `json:"absent,omitempty"`    // the query must return NO candidates
	MinScore float64 `json:"min_score,omitempty"` // with ExpectID: candidate's score must be >= this (0 = any)
}

// FilterField declares one payload dot-path as query-filterable under a
// logical Name (e.g. Name "program", Path "meta.program"). Declarations are
// part of the resolved configuration: they are digested into the list's
// version stamp, so a declaration change is an identity change. NewList
// normalizes the slice to unique, name-sorted entries — declaration ORDER
// is never identity-bearing.
type FilterField struct {
	Name string `json:"name"` // the logical name queries use
	Path string `json:"path"` // dot-path into the payload (array auto-descent)
}

// ListConfig is a list's full declared configuration.
type ListConfig struct {
	Analyzer AnalyzerConfig `json:"analyzer"`
	Match    MatchConfig    `json:"match"`

	// Golden is the list's readiness probe set (see GoldenProbe); empty
	// means the list contributes only its load state to readiness.
	Golden []GoldenProbe `json:"golden,omitempty"`

	// Filterable declares which payload dot-paths queries may filter on,
	// under logical names (see FilterField). Empty (the default) means the
	// list accepts no filters. NewList validates and normalizes the slice
	// (unique, name-sorted); a declaration change is a config-identity
	// change.
	Filterable []FilterField `json:"filterable,omitempty"`

	// OverlayAutoCompact, when > 0, makes the Store fold the overlay into a
	// new base in the background once the overlay reaches this many entries;
	// 0 (default) disables. Decision: an absolute entry count, not a
	// percentage of the base — the governed cost (the O(overlay) rebuild on
	// every mutation) depends only on overlay size. Caveat: compaction
	// recomputes segment-local IDF over the folded corpus, so ngram scores
	// can shift slightly at each auto-trigger (see the score-semantics note
	// in the README). Ignored by library-managed lists (no Store, no disk to
	// fold onto).
	OverlayAutoCompact int `json:"overlay_auto_compact,omitempty"`
}

// clone returns a copy sharing no mutable state with the receiver. Four
// fields are slice-backed (analyzer steps, gram sizes, golden probes,
// filterable fields — GoldenProbe and FilterField themselves are all
// scalars); everything else copies by value.
// NewList clones on the way in and Config on the way out, so a list's
// validated config can never be reached through a caller-held slice.
func (c ListConfig) clone() ListConfig {
	c.Analyzer.Steps = slices.Clone(c.Analyzer.Steps)
	c.Match.Grams = slices.Clone(c.Match.Grams)
	c.Golden = slices.Clone(c.Golden)
	c.Filterable = slices.Clone(c.Filterable)
	return c
}
