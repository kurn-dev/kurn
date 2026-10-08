package server

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestStoreErrStatusReadOnlyDefense(t *testing.T) {
	if got := storeErrStatus(fmt.Errorf("wrapped: %w", engine.ErrReadOnly)); got != http.StatusConflict {
		t.Fatalf("got %d, want 409", got)
	}
}
