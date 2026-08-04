package analyzer_test

import (
	"sync"
	"testing"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"github.com/kurn-dev/kurn/engine/analyzer"
)

// Mixed workload: mostly ASCII (the common case for person names) with a
// diacritic minority, matching real corpora.
var normalizeInputs = []string{
	"John Smith",
	"MARIA GARCIA-LOPEZ",
	"Dr. Robert Brown Jr.",
	"anna kowalski",
	"José Álvarez",
	"François Müller",
	"Zoë Çelik",
	"Peter O'Neil",
	"liu wei",
	"NGUYEN Thi Minh",
}

// TestFoldDiacriticsEquivalence pins the optimized fold_diacritics (ASCII fast
// path + pooled chains) to the straightforward baseline: a freshly built
// transform.Chain per string.
func TestFoldDiacriticsEquivalence(t *testing.T) {
	a, err := analyzer.New([]string{"fold_diacritics"})
	if err != nil {
		t.Fatal(err)
	}
	inputs := append([]string{
		"", " ", "cafe", "café", "ÀÉÎÕÜ àéîõü", "ñoño", "šžõäöü",
		"Ærøskøbing", "ßharp", "Łódź", "naïve façade", "no-marks-at-all 123",
		"ééééééééééééééééééééééééééééééééééééééééééééééééééééééééééééééé",
	}, normalizeInputs...)
	for _, in := range inputs {
		ref := freshFold(in)
		// The analyzer collapses spaces after every step list; apply the same
		// to the baseline so we compare only the fold behavior.
		if got, want := a.Normalize(in), collapse(ref); got != want {
			t.Errorf("fold_diacritics(%q) = %q, want %q", in, got, want)
		}
	}
}

// freshFold is the pre-optimization implementation: new chain per call.
func freshFold(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, _ := transform.String(t, s)
	return out
}

func collapse(s string) string {
	a, _ := analyzer.New([]string{"trim"})
	return a.Normalize(s)
}

// TestNormalizeConcurrent pins the fold-chain pool's race safety: transform
// chains carry internal buffers, so Normalize must never share one across
// goroutines. Meaningful under -race; also checks outputs under contention
// (a shared chain would corrupt results, not just trip the race detector).
func TestNormalizeConcurrent(t *testing.T) {
	a, err := analyzer.Preset("person-name")
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, len(normalizeInputs))
	for i, in := range normalizeInputs {
		want[i] = a.Normalize(in)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for k := 0; k < 400; k++ {
				i := (g + k) % len(normalizeInputs)
				if got := a.Normalize(normalizeInputs[i]); got != want[i] {
					t.Errorf("goroutine %d: Normalize(%q) = %q, want %q", g, normalizeInputs[i], got, want[i])
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

func BenchmarkNormalize(b *testing.B) {
	a, err := analyzer.Preset("person-name")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.Normalize(normalizeInputs[i%len(normalizeInputs)])
	}
}
