package desktop

import (
	"context"
	"time"
)

func waitForNoOtherInstance(ctx context.Context, interval time.Duration, scan func() (bool, error)) error {
	if interval <= 0 {
		interval = 50 * time.Millisecond
	}
	for {
		active, err := scan()
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}
