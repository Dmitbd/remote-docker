//go:build !windows

package desktop

import (
	"context"
	"errors"
	"time"
)

func WaitForNoOtherInstance(context.Context, string, time.Duration) error {
	return errors.New("desktop process verification is only available on Windows")
}
