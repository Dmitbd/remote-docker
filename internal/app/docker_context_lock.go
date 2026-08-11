package app

import "context"

type dockerContextLocker interface {
	WithLock(context.Context, func() error) error
}
