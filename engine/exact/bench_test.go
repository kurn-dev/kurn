package exact_test

import (
	"fmt"
	"testing"

	"github.com/kurn-dev/kurn/engine/exact"
)

// benchKeys builds n entries, one key each, plus a shared key every 64th
// entry so the multi-ordinal (overflow) path is exercised too.
func benchKeys(n int) [][]string {
	keys := make([][]string, n)
	for i := 0; i < n; i++ {
		keys[i] = []string{fmt.Sprintf("bench-key-%08d", i)}
		if i%64 == 0 {
			keys[i] = append(keys[i], fmt.Sprintf("shared-%d", (i/64)%16))
		}
	}
	return keys
}

func benchIndex(n int) *exact.Index {
	b := exact.NewBuilder()
	for i, ks := range benchKeys(n) {
		b.Add(uint32(i), ks)
	}
	idx, err := b.Finish()
	if err != nil {
		panic(err) // bench data is far below the run cap
	}
	return idx
}

func BenchmarkFinish(b *testing.B) {
	const n = 200_000
	keys := benchKeys(n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bld := exact.NewBuilder()
		for ord, ks := range keys {
			bld.Add(uint32(ord), ks)
		}
		idx, err := bld.Finish()
		if err != nil {
			b.Fatal(err)
		}
		if idx.Keys() == 0 {
			b.Fatal("empty index")
		}
	}
}

func BenchmarkLookup(b *testing.B) {
	idx := benchIndex(200_000)
	b.Run("hit-single", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if got := idx.Lookup("bench-key-00123457"); len(got) != 1 {
				b.Fatalf("got %v", got)
			}
		}
	})
	b.Run("hit-multi", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if got := idx.Lookup("shared-0"); len(got) < 2 {
				b.Fatalf("got %v", got)
			}
		}
	})
	b.Run("miss", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if got := idx.Lookup("bench-key-99999999"); got != nil {
				b.Fatalf("got %v", got)
			}
		}
	})
}
