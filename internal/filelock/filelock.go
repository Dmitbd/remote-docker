// Package filelock provides crash-released, cross-process file locks.
package filelock

import (
	"context"
	"errors"
	"time"
)

var ErrLocked = errors.New("file lock is already held")

const retryInterval = 10 * time.Millisecond

func Acquire(ctx context.Context, path string) (*Lock, error) {
	for {
		lock, err := TryAcquire(path)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, ErrLocked) {
			return nil, err
		}
		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
