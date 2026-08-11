//go:build !windows

package filelock

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type Lock struct{ file *os.File }

func TryAcquire(path string) (*Lock, error) {
	file, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, errors.New("lock file")
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
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
