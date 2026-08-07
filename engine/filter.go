package engine

// Query-time payload filters: compilation and evaluation. A filter is a
// map from DECLARED logical names (ListConfig.Filterable) to expected
// decoded-string values, ANDed. Compilation validates names against the
// list's declarations, clones caller data, sorts names (deterministic
// errors and evidence), and splits each declared dot-path once. Evaluation
// happens per score-qualifying candidate, before the top-K cut — see the
// iteration-5 study for why post-cut filtering is a recall bug.

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// maxFilterValueRunes bounds a filter value's length (mirrors the query
// length bound; values are compared as exact decoded strings).
const maxFilterValueRunes = 512

// filterTerm is one compiled AND term.
type filterTerm struct {
	name      string   // logical name (for evidence/errors)
	segs      []string // declared dot-path, split once
	want      string   // expected decoded string
	wantClean bool     // want needs no JSON escaping (fast-path eligible)
	segsClean bool     // every path segment needs no JSON escaping
}

// compiledFilter is the immutable, name-sorted prepared form of a request
// filter against one list's declarations. Terms share one path trie; a
// single strict traversal evaluates every term (one-pass multi-path).
type compiledFilter struct {
	terms []filterTerm // name-sorted, for the applied() echo
	root  *filterNode
	full  uint8 // all-terms-matched mask
}

// eval reports whether payload passes every term, in ONE traversal of the
// payload. A malformed payload is an error, never a non-match: a dropped
// candidate could otherwise manufacture a clear result (the predicate
// captures it; the query aborts).
func (f *compiledFilter) eval(payload json.RawMessage) (bool, error) {
	s := &payloadScan{b: payload}
	var bits uint8
	if err := s.walkValue(f.root, &bits); err != nil {
		return false, err
	}
	s.ws()
	if s.i != len(s.b) {
		return false, &scanError{"trailing data"}
	}
	return bits == f.full, nil
}

// applied returns the normalized applied map for the response echo:
// name-sorted, one value per name.
func (f *compiledFilter) applied() map[string]string {
	if f == nil {
		return nil
	}
	out := make(map[string]string, len(f.terms))
	for _, t := range f.terms {
		out[t.name] = t.want
	}
	return out
}

// compileFilter validates and compiles a request filter against the list's
// declarations. A nil or empty filter compiles to (nil, nil) — the
// no-filter path. An undeclared name is an error naming it (fail-closed:
// a typo'd filter must never silently clear a screening answer).
func (l *List) compileFilter(filter map[string]string) (*compiledFilter, error) {
	if len(filter) == 0 {
		return nil, nil
	}
	if len(filter) > maxFilterFields {
		return nil, fmt.Errorf("list %s: filter has %d names, max %d", l.name, len(filter), maxFilterFields)
	}
	// Sort names BEFORE any validation: errors and evidence must name the
	// same offender on every call — map iteration order is nondeterministic.
	names := make([]string, 0, len(filter))
	for name := range filter {
		names = append(names, name)
	}
	sort.Strings(names)
	cf := &compiledFilter{terms: make([]filterTerm, 0, len(names))}
	for _, name := range names {
		if utf8.RuneCountInString(name) > maxFilterNameRunes {
			return nil, fmt.Errorf("list %s: filter name %q exceeds %d chars", l.name, name, maxFilterNameRunes)
		}
		if utf8.RuneCountInString(filter[name]) > maxFilterValueRunes {
			return nil, fmt.Errorf("list %s: filter value for %q exceeds %d chars", l.name, name, maxFilterValueRunes)
		}
		i, found := slices.BinarySearchFunc(l.cfg.Filterable, name, func(f FilterField, target string) int {
			return strings.Compare(f.Name, target)
		})
		if !found {
			return nil, fmt.Errorf("list %s: filter name %q is not declared filterable", l.name, name)
		}
		segs := strings.Split(l.cfg.Filterable[i].Path, ".")
		segsClean := true
		for _, sg := range segs {
			if !cleanASCII(sg) {
				segsClean = false
				break
			}
		}
		cf.terms = append(cf.terms, filterTerm{
			name:      name,
			segs:      segs,
			want:      filter[name],
			wantClean: cleanASCII(filter[name]),
			segsClean: segsClean,
		})
	}
	// Build the shared path trie: one traversal of a payload evaluates all
	// terms. Bit i belongs to terms[i]; full is the all-terms mask.
	cf.root = &filterNode{}
	for idx, t := range cf.terms {
		n := cf.root
		for _, seg := range t.segs {
			if n.children == nil {
				n.children = map[string]*filterNode{}
				n.clean = map[string]bool{}
			}
			child, ok := n.children[seg]
			if !ok {
				child = &filterNode{}
				n.children[seg] = child
				n.clean[seg] = cleanASCII(seg)
			}
			n = child
		}
		n.terms = append(n.terms, terminalTerm{bit: 1 << uint(idx), want: t.want, wantClean: t.wantClean})
	}
	cf.full = uint8(1<<uint(len(cf.terms))) - 1
	return cf, nil
}
