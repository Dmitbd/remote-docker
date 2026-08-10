package credentials

import (
	"errors"
	"fmt"

	keyring "github.com/zalando/go-keyring"
)

const keyringService = "io.github.dmitbd.remote-docker"

// KeyringStore persists secrets in the operating system credential manager.
type KeyringStore struct{}

// NewKeyringStore returns the production credential store.
func NewKeyringStore() *KeyringStore {
	return &KeyringStore{}
}

// Put stores a secret in macOS Keychain or Windows Credential Manager.
func (s *KeyringStore) Put(deviceID, name string, value []byte) error {
	key, err := account(deviceID, name)
	if err != nil {
		return err
	}
	if err := keyring.Set(keyringService, key, string(value)); err != nil {
		return fmt.Errorf("store credential: %w", err)
	}
	return nil
}

// Get reads a secret from the operating system credential manager.
func (s *KeyringStore) Get(deviceID, name string) ([]byte, error) {
	key, err := account(deviceID, name)
	if err != nil {
		return nil, err
	}
	value, err := keyring.Get(keyringService, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("read credential: %w", err)
	}
	return []byte(value), nil
}

// Delete removes a secret from the operating system credential manager.
func (s *KeyringStore) Delete(deviceID, name string) error {
	key, err := account(deviceID, name)
	if err != nil {
		return err
	}
	if err := keyring.Delete(keyringService, key); errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	} else if err != nil {
		return fmt.Errorf("delete credential: %w", err)
	}
	return nil
}

var _ Store = (*KeyringStore)(nil)
