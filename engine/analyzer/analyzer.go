// Package analyzer is the single canonicalization pipeline shared by the index
// builder and the query path. Both sides MUST run the same analyzer so a key
// normalizes identically whether indexed or looked up.
package analyzer

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

type stepFn func(string) string

// Analyzer applies an ordered list of normalization steps.
type Analyzer struct {
	steps []stepFn
	names []string // canonical step spec, for config digests
}

// New builds an analyzer from step names. Supported: lowercase,
// fold_diacritics, strip_punctuation, strip_words:<w1,w2,...>, sort_tokens,
// trim, idna.
func New(steps []string) (Analyzer, error) {
	a := Analyzer{names: append([]string(nil), steps...)}
	for _, s := range steps {
		name, arg, hasArg := strings.Cut(s, ":")
		// Arg-less steps must reject arguments (including an empty one after
		// the colon): "lowercase:junk" silently parsing as "lowercase" would
		// let behaviorally identical analyzers differ in canonical spec — and
		// therefore in config digest.
		if hasArg && name != "strip_words" {
			return Analyzer{}, fmt.Errorf("analyzer: step %q takes no argument", s)
		}
		switch name {
		case "lowercase":
			a.steps = append(a.steps, strings.ToLower)
		case "fold_diacritics":
			a.steps = append(a.steps, foldDiacritics)
		case "strip_punctuation":
			a.steps = append(a.steps, stripPunctuation)
		case "strip_words":
			words := map[string]bool{}
			for _, w := range strings.Split(arg, ",") {
				if w = strings.TrimSpace(w); w != "" {
					words[w] = true
				}
			}
			if len(words) == 0 {
				return Analyzer{}, fmt.Errorf("analyzer: step %q has an empty word set", s)
			}
			a.steps = append(a.steps, stripWords(words))
		case "sort_tokens":
			a.steps = append(a.steps, sortTokens)
		case "trim":
			a.steps = append(a.steps, collapseSpaces)
		case "idna":
			a.steps = append(a.steps, idnaStep)
		default:
			return Analyzer{}, fmt.Errorf("analyzer: unknown step %q", s)
		}
	}
	return a, nil
}

// Steps returns the canonical step spec (for config digests / persistence).
// The slice is a copy: callers may mutate it freely.
func (a Analyzer) Steps() []string { return append([]string(nil), a.names...) }

func (a Analyzer) Normalize(s string) string {
	for _, f := range a.steps {
		s = f(s)
	}
	return collapseSpaces(s)
}

// foldChains pools NFD→strip-Mn→NFC transform chains for reuse. Transformers
// carry internal buffers, so a single shared package-level chain would be a
// data race under concurrent Normalize calls; pooling gives each goroutine an
// exclusive chain and amortizes the (expensive) chain construction.
var foldChains = sync.Pool{
	New: func() any {
		return transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	},
}

// foldDiacritics: NFD decomposes accented letters into base + combining mark,
// the mark (category Mn) is stripped, NFC recomposes.
//
// Known limitation (inherent to this design-pinned approach): foldables that
// are not base+Mn decompositions — ø, Ł, ß, Đ, æ, … — do not fold, because
// NFD produces no combining marks for them.
func foldDiacritics(s string) string {
	// ASCII fast path: pure-ASCII strings are NFC-normalized and mark-free by
	// construction, so the transform is the identity. The majority of keys
	// take this path and skip the transform machinery entirely.
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	t := foldChains.Get().(transform.Transformer)
	defer foldChains.Put(t)
	out, _, _ := transform.String(t, s) // String resets t before use
	return out
}

// stripPunctuation keeps letters, digits and intra-token hyphens.
func stripPunctuation(s string) string {
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		var b strings.Builder
		for _, r := range f {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' {
				b.WriteRune(r)
			}
		}
		if tok := strings.Trim(b.String(), "-"); tok != "" {
			out = append(out, tok)
		}
	}
	return strings.Join(out, " ")
}

func stripWords(words map[string]bool) stepFn {
	return func(s string) string {
		fields := strings.Fields(s)
		out := make([]string, 0, len(fields))
		for _, f := range fields {
			if !words[strings.Trim(f, ".")] {
				out = append(out, f)
			}
		}
		return strings.Join(out, " ")
	}
}

func sortTokens(s string) string {
	fields := strings.Fields(s)
	sort.Strings(fields)
	return strings.Join(fields, " ")
}

func collapseSpaces(s string) string { return strings.Join(strings.Fields(s), " ") }

// idnaProfile is the lenient lookup mapping: full IDNA case/width mapping and
// punycode conversion, without the strict-DNS character check that the stock
// idna.Lookup profile adds. Blocklists are full of technically-invalid but
// real shapes — underscored service labels (_dmarc.example.com, smtp_1.host),
// wildcard rows (*.tempmail.com) — and under a strict profile those ERROR and
// degrade unconverted, leaving Unicode and punycode spellings of the same
// domain divergent (fatal for parent-suffix probing, which never
// re-analyzes). Strictness belongs to validation, not normalization: here
// every spelling must converge to one indexed form.
//
// Concurrency: idna.New builds the Profile's options struct once and nothing
// mutates it afterwards — ToASCII/process are read-only on the Profile
// (verified against x/net v0.56.0 source, same as the stock idna.Lookup), so
// shared concurrent use from Normalize is safe.
var idnaProfile = idna.New(idna.MapForLookup(), idna.StrictDomainName(false))

// idnaStep normalizes a domain name: strips leading and trailing dots (ALL of
// them — ToASCII preserves a remaining root dot with nil error, so trimming
// just one trailing dot would make "example.com.." normalize to
// "example.com.", breaking idempotence; leading dots are as meaningless as
// trailing ones, and keeping them would make ".tempmail.com" look like a
// subdomain of tempmail.com to parent_domain fallback, scoring the listed
// domain itself 90 instead of 100), then converts internationalized labels to
// punycode via the lenient idnaProfile. A name whose labels are all empty ("", ".", "..", ...)
// normalizes to "" — no domain survives. The error-degrade branch is a safety
// net for the rare Unicode inputs even the lenient profile rejects (e.g.
// ContextJ violations like a zero-width non-joiner inside a label): degrade
// to the dot-stripped input rather than erroring — Normalize is
// string -> string, and because index and query run the identical pipeline,
// degraded keys still match themselves; an unmappable query simply matches
// nothing.
func idnaStep(s string) string {
	s = strings.Trim(s, ".")
	if s == "" {
		return ""
	}
	if out, err := idnaProfile.ToASCII(s); err == nil {
		return out
	}
	return s
}

// Presets are named step lists; a preset is nothing an explicit step list
// couldn't express.
var presets = map[string][]string{
	"person-name": {"lowercase", "fold_diacritics", "strip_punctuation", "strip_words:mr,mrs,ms,dr,prof", "sort_tokens"},
	"identifier":  {"lowercase", "trim"},
	"domain":      {"lowercase", "trim", "idna"},
	"free-text":   {"lowercase", "fold_diacritics", "strip_punctuation"},
}

// Preset returns the analyzer for a named preset.
func Preset(name string) (Analyzer, error) {
	steps, ok := presets[name]
	if !ok {
		return Analyzer{}, fmt.Errorf("analyzer: unknown preset %q", name)
	}
	return New(steps)
}

// PresetSteps exposes a preset's step list (for persisting resolved config).
// The slice is a copy: mutating it cannot corrupt the global preset table.
func PresetSteps(name string) ([]string, bool) {
	s, ok := presets[name]
	if !ok {
		return nil, false
	}
	return append([]string(nil), s...), true
}
