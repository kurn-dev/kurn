package server

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestDamagedMutationMapsToConflict(t *testing.T) {
	err := fmt.Errorf("mutation refused: %w", engine.ErrListDamaged)
	if got := storeErrStatus(err); got != http.StatusConflict {
		t.Fatalf("storeErrStatus(ErrListDamaged) = %d, want 409", got)
	}
}

func TestFirstCreateDamageInvalidatesReadyCache(t *testing.T) {
	cached := map[string]readyKey{} // a green result cached before the failure
	current := map[string]readyKey{}
	const repair = "PUT-recreate the list"
	addReadyKeys(current, "", nil, []engine.DamagedList{{List: "people", Repair: repair}})

	if keysMatch(cached, current) {
		t.Fatal("damaged first-create placeholder did not invalidate the ready cache")
	}
	key, ok := current["/people"]
	if !ok || key.l != nil || key.version != "" || key.damage != repair {
		t.Fatalf("placeholder ready key = %+v, present=%v", key, ok)
	}
}
