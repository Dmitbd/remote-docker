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

type fileStateLocker struct{ path string }

func newStateLocker(path string) stateLocker {
	return fileStateLocker{path: path}
}

func (l fileStateLocker) WithLock(ctx context.Context, operation func() error) error {
	if !filepath.IsAbs(l.path) {
		return errors.New("state lock path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return errors.New("create state lock directory")
	}
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return errors.New("open state lock")
	}
	defer file.Close()

	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return errors.New("acquire state lock")
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
