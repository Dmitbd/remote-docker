//go:build windows

package desktop

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"golang.org/x/sys/windows"
)

type mutexInstanceLock struct {
	handle windows.Handle
}

func AcquireSingleInstance(scope string) (InstanceLock, error) {
	digest := sha256.Sum256([]byte(scope))
	name, err := windows.UTF16PtrFromString(`Local\RemoteDocker-` + hex.EncodeToString(digest[:16]))
	if err != nil {
		return nil, errors.New("create single-instance name")
	}
	handle, err := windows.CreateMutex(nil, true, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(handle)
		return nil, ErrAlreadyRunning
	}
	if err != nil {
		return nil, errors.New("create single-instance mutex")
	}
	return &mutexInstanceLock{handle: handle}, nil
}

func (l *mutexInstanceLock) Close() error {
	if l == nil || l.handle == 0 {
		return nil
	}
	_ = windows.ReleaseMutex(l.handle)
	err := windows.CloseHandle(l.handle)
	l.handle = 0
	return err
}
