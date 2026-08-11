//go:build !windows

package localapi

import (
	"errors"
	"os"
	"syscall"
)

// IsEndpointAbsent reports only errors that prove no local control socket exists.
func IsEndpointAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}
