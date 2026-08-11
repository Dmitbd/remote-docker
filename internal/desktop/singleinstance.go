package desktop

import (
	"errors"
	"io"

	"github.com/Dmitbd/remote-docker/internal/filelock"
)

var ErrAlreadyRunning = errors.New("Remote Docker is already running")

type InstanceLock interface {
	io.Closer
}

func AcquireSingleInstance(path string) (InstanceLock, error) {
	lock, err := filelock.TryAcquire(path)
	if errors.Is(err, filelock.ErrLocked) {
		return nil, ErrAlreadyRunning
	}
	if err != nil {
		return nil, err
	}
	return lock, nil
}
