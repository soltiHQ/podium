package inmemory_test

import (
	"testing"

	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/storagecontract"
)

// TestContract runs the storage contract on the in-memory store, the reference
// every other backend is held to.
func TestContract(t *testing.T) {
	storagecontract.Run(t, func(*testing.T) storage.Storage { return inmemory.New() })
}
