package engine

// Query-time payload filters: compilation and evaluation. Typed expressions
// map DECLARED logical names (ListConfig.Filterable) to type-exact scalar
// equality or canonical IN sets; names are ANDed. The legacy string map is a
// wrapper over that representation. Compilation validates declarations,
// sorts names (deterministic errors/evidence), and splits each declared path
// once. Evaluation happens per score-qualifying candidate before top-K — see
// the iteration-5 study for why post-cut filtering is a recall bug.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// maxFilterValueRunes bounds a filter value's length (mirrors the query
// length bound; values are compared as exact decoded strings).
const maxFilterValueRunes = 512

// filterTerm is one compiled AND term.
type filterTerm struct {
	name      string   // logical name (for evidence/errors)
	segs      []string // declared dot-path, split once
	value     filterValue
	segsClean bool // every path segment needs no JSON escaping
}

// compiledFilter is the immutable, name-sorted prepared form of a request
// filter against one list's declarations. Terms share one path trie; a
// single strict traversal evaluates every term (one-pass multi-path).
type compiledFilter struct {
	terms        []filterTerm // name-sorted
	root         *filterNode
	full         uint8 // all-terms-matched mask
	canonical    []byte
	alternatives int
	scalarBytes  int
	legacy       map[string]string // non-nil only for the v0.4.0 wrapper
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

// applied returns a defensive copy for legacy callers. Typed callers use the
// canonical JSON echo instead.
func (f *compiledFilter) applied() map[string]string {
	if f == nil || f.legacy == nil {
		return nil
	}
	out := make(map[string]string, len(f.legacy))
	for name, value := range f.legacy {
		out[name] = value
	}
	return out
}

func (f *compiledFilter) appliedJSON() []byte {
	if f == nil {
		return nil
	}
	return slices.Clone(f.canonical)
}

// compileFilter validates and compiles a request filter against the list's
// declarations. A nil or empty filter compiles to (nil, nil) — the
// no-filter path. An undeclared name is an error naming it (fail-closed:
// a typo'd filter must never silently clear a screening answer).
func (l *List) compileFilter(filter map[string]string) (*compiledFilter, error) {
	expr, err := StringTypedFilter(filter)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", l.name, err)
	}
	cf, err := l.compileTypedFilter(expr)
	if err != nil || cf == nil {
		return cf, err
	}
	cf.legacy = make(map[string]string, len(filter))
	for name, value := range filter {
		cf.legacy[name] = value
	}
	return cf, nil
}

// compileTypedFilter resolves an immutable typed expression against this
// list's declarations and builds the shared one-pass path trie.
func (l *List) compileTypedFilter(filter TypedFilter) (*compiledFilter, error) {
	if filter.Empty() {
		return nil, nil
	}
	cf := &compiledFilter{
		terms:        make([]filterTerm, 0, len(filter.terms)),
		canonical:    slices.Clone(filter.canonical),
		alternatives: filter.alternatives,
		scalarBytes:  filter.scalarBytes,
	}
	for _, term := range filter.terms {
		name := term.name
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
			value:     term.value,
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
		n.terms = append(n.terms, terminalTerm{bit: 1 << uint(idx), value: t.value})
	}
	cf.full = uint8(1<<uint(len(cf.terms))) - 1
	return cf, nil
}
