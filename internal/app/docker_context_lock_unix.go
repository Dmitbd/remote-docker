//go:build !windows

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type fileDockerContextLocker struct{ path string }

func newDockerContextLocker(path string) dockerContextLocker {
	return fileDockerContextLocker{path: path}
}

func (l fileDockerContextLocker) WithLock(ctx context.Context, operation func() error) error {
	if !filepath.IsAbs(l.path) {
		return errors.New("Docker context lock path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return errors.New("create Docker context lock directory")
	}
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return errors.New("open Docker context lock")
	}
	defer file.Close()

	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return errors.New("acquire Docker context lock")
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN) //nolint:errcheck
	if operation == nil {
		return nil
	}
	return operation()
}
