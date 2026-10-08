//go:build !unix

package prepared

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Without the unix build tag the mapped loader is unsupported, and refuses
// before touching the file or the registry.
func TestOpenMappedUnsupported(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.kart")
	if err := os.WriteFile(p, artifactT(t, "ngram", 10), 0o444); err != nil {
		t.Fatal(err)
	}
	v, ver, err := OpenMapped(p, LevelFull, nil)
	if v != nil || ver != nil || !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("OpenMapped: %v %v %v, want ErrUnsupported", v, ver, err)
	}
	if procMapped.adm != nil {
		t.Fatal("unsupported open admitted a mapping")
	}
}
