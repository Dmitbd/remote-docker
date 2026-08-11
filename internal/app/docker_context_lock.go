package app

import "context"

type stateLocker interface {
	WithLock(context.Context, func() error) error
}
