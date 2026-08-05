// The dot-path resolver: one tiny walker over a format-neutral node model
// (*xnode for XML, map[string]any/[]any/json.Number for JSON, plain
// map[string]string for CSV rows). Deliberately hand-rolled: no streaming-
// XML JSONPath library exists to reuse, and equals-only filters keep this
// a page of code, not an expression language.
package ingest

import (
	"strings"
)

// splitPath turns a mapping path into segments. CSV paths are column
// names used VERBATIM (a header may legally contain dots); everything
// else splits on dots.
func splitPath(format, path string) []string {
	if format == "csv" {
		return []string{path}
	}
	return strings.Split(path, ".")
}

// xnode is a decoded XML element: direct child elements by local name,
// plus the element's own character data.
type xnode struct {
	children map[string][]*xnode
	text     string
	buf      []byte // character data during decode; folded into text at EndElement
}

// kids returns the child nodes one segment reaches from n. Arrays
// auto-descend: resolving "a.b" over {"a":[{"b":1},{"b":2}]} yields both.
func kids(n any, seg string) []any {
	switch v := n.(type) {
	case *xnode:
		out := make([]any, 0, len(v.children[seg]))
		for _, c := range v.children[seg] {
			out = append(out, c)
		}
		return out
	case map[string]any:
		c, ok := v[seg]
		if !ok {
			return nil
		}
		if arr, isArr := c.([]any); isArr {
			return arr
		}
		return []any{c}
	case map[string]string:
		c, ok := v[seg]
		if !ok {
			return nil
		}
		return []any{c}
	case []any:
		var out []any
		for _, e := range v {
			out = append(out, kids(e, seg)...)
		}
		return out
	}
	return nil
}

// resolve walks segs from root, fanning out at every repeated element.
func resolve(root any, segs []string) []any {
	ctx := []any{root}
	for _, seg := range segs {
		var next []any
		for _, n := range ctx {
			next = append(next, kids(n, seg)...)
		}
		if len(next) == 0 {
			return nil
		}
		ctx = next
	}
	return ctx
}

// text renders a scalar node; non-scalars render "" (payload and key
// paths should point at scalars — a "" simply contributes nothing).
func text(n any) string {
	switch v := n.(type) {
	case *xnode:
		return strings.TrimSpace(v.text)
	case string:
		return strings.TrimSpace(v)
	case interface{ String() string }: // json.Number keeps source digits
		return v.String()
	case bool:
		if v {
			return "true"
		}
		return "false"
	case nil:
		return ""
	}
	return ""
}

// texts renders all scalar matches, empties dropped.
func texts(nodes []any) []string {
	var out []string
	for _, n := range nodes {
		if t := text(n); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func anyEquals(nodes []any, want string) bool {
	for _, n := range nodes {
		t := text(n)
		if want == "*" {
			if strings.TrimSpace(t) != "" {
				return true
			}
			continue
		}
		if t == want {
			return true
		}
	}
	return false
}

// applyRule emits the rule's keys for one record. The instance set is the
// deepest common segment prefix across the rule's paths AND where paths —
// filters and joined components are evaluated per instance (per aka
// element, per row object), never across siblings.
func applyRule(doc any, format string, rule KeyRule) []string {
	paths := make([][]string, 0, len(rule.paths()))
	for _, p := range rule.paths() {
		paths = append(paths, splitPath(format, p))
	}
	wherePaths := make(map[string][]string, len(rule.Where))
	all := make([][]string, 0, len(paths)+len(rule.Where))
	all = append(all, paths...)
	for wp := range rule.Where {
		segs := splitPath(format, wp)
		wherePaths[wp] = segs
		all = append(all, segs)
	}
	common := commonPrefix(all)

	var keys []string
	for _, inst := range resolve(doc, common) {
		ok := true
		for wp, want := range rule.Where {
			if !anyEquals(resolve(inst, wherePaths[wp][len(common):]), want) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		var parts []string
		for _, p := range paths {
			if vals := texts(resolve(inst, p[len(common):])); len(vals) > 0 {
				parts = append(parts, vals[0])
			}
		}
		k := strings.TrimSpace(strings.Join(parts, rule.join()))
		if k == "" {
			continue
		}
		if rule.Split == "" {
			keys = append(keys, k)
			continue
		}
		for _, piece := range strings.Split(k, rule.Split) {
			if p := strings.TrimSpace(piece); p != "" {
				keys = append(keys, p)
			}
		}
	}
	return keys
}

// commonPrefix returns the longest segment prefix shared by every path.
func commonPrefix(paths [][]string) []string {
	if len(paths) == 0 {
		return nil
	}
	prefix := paths[0]
	for _, p := range paths[1:] {
		n := 0
		for n < len(prefix) && n < len(p) && prefix[n] == p[n] {
			n++
		}
		prefix = prefix[:n]
	}
	return prefix
}
