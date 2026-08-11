//go:build windows

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"golang.org/x/sys/windows"
)

type mutexDockerContextLocker struct{ scope string }

func newDockerContextLocker(scope string) dockerContextLocker {
	return mutexDockerContextLocker{scope: scope}
}

func (l mutexDockerContextLocker) WithLock(ctx context.Context, operation func() error) error {
	digest := sha256.Sum256([]byte(l.scope))
	name, err := windows.UTF16PtrFromString(`Local\RemoteDocker-Context-` + hex.EncodeToString(digest[:16]))
	if err != nil {
		return errors.New("create Docker context lock name")
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		return errors.New("create Docker context lock")
	}
	defer windows.CloseHandle(handle)
	for {
		status, waitErr := windows.WaitForSingleObject(handle, 10)
		if waitErr != nil {
			return errors.New("acquire Docker context lock")
		}
		if status == windows.WAIT_OBJECT_0 || status == windows.WAIT_ABANDONED {
			break
		}
		if status != uint32(windows.WAIT_TIMEOUT) {
			return errors.New("acquire Docker context lock")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	defer windows.ReleaseMutex(handle) //nolint:errcheck
	if operation == nil {
		return nil
	}
	return operation()
}
