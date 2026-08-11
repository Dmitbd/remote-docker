//go:build windows

package localapi

import (
	"errors"

	"golang.org/x/sys/windows"
)

// IsEndpointAbsent reports only errors that prove no local control pipe exists.
func IsEndpointAbsent(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}
