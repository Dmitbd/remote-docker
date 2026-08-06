package credentials

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrNotFound reports that the requested secret does not exist.
	ErrNotFound = errors.New("credential not found")
	// ErrInvalidKey reports an unsafe or incomplete device/name pair.
	ErrInvalidKey = errors.New("invalid credential key")
)

// Store persists secrets under a device-specific namespace.
type Store interface {
	Put(deviceID, name string, value []byte) error
	Get(deviceID, name string) ([]byte, error)
	Delete(deviceID, name string) error
}

func account(deviceID, name string) (string, error) {
	if deviceID == "" || name == "" || strings.Contains(deviceID, "/") || strings.Contains(name, "/") {
		return "", fmt.Errorf("%w: device ID and name must be non-empty and cannot contain '/'", ErrInvalidKey)
	}

	return deviceID + "/" + name, nil
}
