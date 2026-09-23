package store

import "testing"

func TestMemoryStoreSatisfiesContract(t *testing.T) {
	storeContract(t, func(t *testing.T) SessionStore { return NewMemoryStore() })
}
