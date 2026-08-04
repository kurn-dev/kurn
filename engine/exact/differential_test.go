package exact_test

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"github.com/kurn-dev/kurn/engine/exact"
)

// Differential: the compact index must return exactly what a plain
// map[string][]uint32 reference returns, for every key ever inserted and
// for misses, across single-ord, multi-ord, and duplicate-key shapes.
func TestExactDifferential(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	ref := map[string][]uint32{}
	b := exact.NewBuilder()
	for ord := uint32(0); ord < 5000; ord++ {
		nKeys := 1 + r.Intn(3)
		keys := make([]string, 0, nKeys)
		for k := 0; k < nKeys; k++ {
			var key string
			if r.Intn(10) == 0 && ord > 0 {
				key = fmt.Sprintf("shared-%d", r.Intn(50)) // hot shared keys -> overflow path
			} else {
				key = fmt.Sprintf("key-%d-%d", ord, k)
			}
			keys = append(keys, key)
			if p := ref[key]; len(p) == 0 || p[len(p)-1] != ord {
				ref[key] = append(ref[key], ord)
			}
		}
		keys = append(keys, keys[0]) // duplicate within one Add
		b.Add(ord, keys)
	}
	idx := mustFinish(t, b)
	if idx.Keys() != len(ref) {
		t.Fatalf("Keys() = %d, want %d", idx.Keys(), len(ref))
	}
	for key, want := range ref {
		got := idx.Lookup(key)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Lookup(%q) = %v, want %v", key, got, want)
		}
	}
	// Append-clobber pass: Lookup's results share one backing array, and only
	// the full-cap subslice it returns keeps a caller's append from
	// overwriting the adjacent key's run. Append garbage through every result
	// (crossing every run boundary regardless of map iteration order), then
	// re-verify everything.
	for key := range ref {
		_ = append(idx.Lookup(key), 0xdeadbeef)
	}
	for key, want := range ref {
		if got := idx.Lookup(key); !reflect.DeepEqual(got, want) {
			t.Errorf("after append: Lookup(%q) = %v, want %v", key, got, want)
		}
	}
	for _, miss := range []string{"", "nope", "key-99999-0"} {
		if got := idx.Lookup(miss); got != nil {
			t.Errorf("Lookup(%q) = %v, want nil", miss, got)
		}
	}
}
