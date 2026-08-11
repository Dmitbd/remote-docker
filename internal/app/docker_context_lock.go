package app

import (
	"context"

	"github.com/Dmitbd/remote-docker/internal/filelock"
)

type stateLocker interface {
	WithLock(context.Context, func() error) error
}

type fileStateLocker struct{ path string }

func newStateLocker(path string) stateLocker {
	return fileStateLocker{path: path}
}

func (l fileStateLocker) WithLock(ctx context.Context, operation func() error) error {
	lock, err := filelock.Acquire(ctx, l.path)
	if err != nil {
		return err
	}
	defer lock.Close()
	if operation == nil {
		return nil
	}
	return operation()
}
