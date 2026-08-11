//go:build windows

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"runtime"

	"golang.org/x/sys/windows"
)

type mutexStateLocker struct{ scope string }

func newStateLocker(scope string) stateLocker {
	return mutexStateLocker{scope: scope}
}

func (l mutexStateLocker) WithLock(ctx context.Context, operation func() error) error {
	// Windows mutex ownership belongs to the calling OS thread. Keep the
	// goroutine pinned through both acquisition and ReleaseMutex.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	digest := sha256.Sum256([]byte(l.scope))
	name, err := windows.UTF16PtrFromString(`Local\RemoteDocker-Context-` + hex.EncodeToString(digest[:16]))
	if err != nil {
		return errors.New("create state lock name")
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		return errors.New("create state lock")
	}
	defer windows.CloseHandle(handle)
	for {
		status, waitErr := windows.WaitForSingleObject(handle, 10)
		if waitErr != nil {
			return errors.New("acquire state lock")
		}
		if status == windows.WAIT_OBJECT_0 || status == windows.WAIT_ABANDONED {
			break
		}
		if status != uint32(windows.WAIT_TIMEOUT) {
			return errors.New("acquire state lock")
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
