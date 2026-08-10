package credentials

import (
	"fmt"
	"sync"
)

// MemoryStore is an in-process Store intended for tests and ephemeral use.
type MemoryStore struct {
	mu      sync.RWMutex
	secrets map[string][]byte
}

// NewMemoryStore returns an empty memory-backed credential store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{secrets: make(map[string][]byte)}
}

// Put stores a private copy of value.
func (s *MemoryStore) Put(deviceID, name string, value []byte) error {
	key, err := account(deviceID, name)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets[key] = append([]byte(nil), value...)
	return nil
}

// Get returns a private copy of a stored value.
func (s *MemoryStore) Get(deviceID, name string) ([]byte, error) {
	key, err := account(deviceID, name)
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.secrets[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}

	return append([]byte(nil), value...), nil
}

// Delete removes one stored value.
func (s *MemoryStore) Delete(deviceID, name string) error {
	key, err := account(deviceID, name)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.secrets[key]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	delete(s.secrets, key)
	return nil
}

var _ Store = (*MemoryStore)(nil)
