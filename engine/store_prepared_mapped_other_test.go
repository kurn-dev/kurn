//go:build !unix

package engine_test

import (
	"errors"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

// Without the unix build tag LoaderMapped is unsupported.
func TestPreparedMappedUnsupported(t *testing.T) {
	path := prepWriteArtifact(t, prepCases()[0])
	st, err := engine.OpenPrepared(path, engine.PreparedOptions{Loader: engine.LoaderMapped})
	if st != nil || !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("LoaderMapped: store %v err %v, want ErrUnsupported", st, err)
	}
}
