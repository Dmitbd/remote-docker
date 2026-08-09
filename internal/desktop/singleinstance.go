package desktop

import (
	"errors"
	"io"
)

var ErrAlreadyRunning = errors.New("Remote Docker is already running")

type InstanceLock interface {
	io.Closer
}
