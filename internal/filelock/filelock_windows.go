//go:build windows

package filelock

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

type Lock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func TryAcquire(path string) (*Lock, error) {
	file, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	lock := &Lock{file: file}
	handle := windows.Handle(file.Fd())
	err = windows.LockFileEx(
		handle,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &lock.overlapped,
	)
	if err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
			return nil, ErrLocked
		}
		return nil, errors.New("lock file")
	}
	return lock, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	handle := windows.Handle(l.file.Fd())
	_ = windows.UnlockFileEx(handle, 0, 1, 0, &l.overlapped)
	err := l.file.Close()
	l.file = nil
	return err
}

func openLockFile(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("file lock path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, errors.New("create file lock directory")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("open file lock")
	}
	return file, nil
}
