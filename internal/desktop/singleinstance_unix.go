//go:build !windows

package desktop

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type fileInstanceLock struct {
	file *os.File
}

func AcquireSingleInstance(path string) (InstanceLock, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("single-instance lock path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, errors.New("create single-instance directory")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("open single-instance lock")
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, errors.New("lock single-instance file")
	}
	return &fileInstanceLock{file: file}, nil
}

func (l *fileInstanceLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}
