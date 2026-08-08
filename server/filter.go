package server

// Query-time filter surface: the top-level duplicate-aware walk of the raw
// "filter" member. The engine's shared typed parser owns the grammar and all
// expression bounds.

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// dupFilterName reports a duplicated logical name inside the request's
// raw "filter" member. encoding/json's map decode collapses duplicate
// keys last-wins, so by the time a map exists the duplicate is gone — a
// screening client could believe it registered one value while the last
// spelling silently won. This token walk sees the raw bytes. Structural
// JSON problems return nil: the ordinary typed decode reports those with
// its own errors.
func dupFilterName(b []byte) error {
	// Fast path: a body containing neither the literal bytes "filter" nor
	// any backslash cannot contain ANY spelling of a filter member (an
	// escaped spelling requires a backslash), so the ordinary unfiltered
	// request skips the token walk entirely. This gate is the whole
	// unfiltered HTTP cost of the feature (see BenchmarkDupFilterWalk):
	// the walk itself outweighs the typed decode, ~1% of a served query.
	// The gate must stay byte-conservative — it may only skip when NO
	// spelling is possible, never because the decoded map looked empty
	// ({"filter":{...},"filter":{}} last-wins to an empty map).
	if !bytes.Contains(b, []byte("filter")) && bytes.IndexByte(b, '\\') < 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	t, err := dec.Token()
	if err != nil {
		return nil
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil
	}
	sawFilter := false
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil
		}
		key, _ := kt.(string)
		if key != "filter" {
			if skipTokenValue(dec) != nil {
				return nil
			}
			continue
		}
		// A repeated MEMBER is worse than a repeated name: the typed
		// decode keeps the LAST value, so {"filter":{...},"filter":{}}
		// (or null) silently erases filtering and takes the unfiltered
		// path with no echo — the exact downgrade duplicate rejection
		// exists to prevent. Reject before reading the second value.
		// (Token returns DECODED keys, so escaped spellings of "filter"
		// are the same member.)
		if sawFilter {
			return fmt.Errorf("duplicate filter member (a repeated member would silently replace the filter)")
		}
		sawFilter = true
		vt, err := dec.Token()
		if err != nil {
			return nil
		}
		d, isDelim := vt.(json.Delim)
		if !isDelim {
			continue // scalar filter: the shared typed parser owns its semantics
		}
		if d == '[' {
			if skipRest(dec) != nil {
				return nil
			}
			continue // array filter: the shared typed parser rejects it
		}
		seen := make(map[string]struct{})
		for dec.More() {
			nt, err := dec.Token()
			if err != nil {
				return nil
			}
			name, _ := nt.(string)
			if _, dup := seen[name]; dup {
				return fmt.Errorf("duplicate filter name %q (a repeated name would silently last-wins)", name)
			}
			seen[name] = struct{}{}
			if skipTokenValue(dec) != nil {
				return nil
			}
		}
		if _, err := dec.Token(); err != nil { // consume the filter's '}'
			return nil
		}
	}
	return nil
}

// skipTokenValue consumes exactly one JSON value from dec.
func skipTokenValue(dec *json.Decoder) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := t.(json.Delim); ok && (d == '{' || d == '[') {
		return skipRest(dec)
	}
	return nil
}

// skipRest consumes tokens until the already-consumed opening delimiter
// is matched.
func skipRest(dec *json.Decoder) error {
	depth := 1
	for depth > 0 {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}
