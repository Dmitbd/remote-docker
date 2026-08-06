package syncer

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// StatusSource supplies the two readiness views without exposing credentials.
type StatusSource interface {
	FolderStatus(context.Context, string) (FolderStatus, error)
	Connections(context.Context) (map[string]ConnectionStatus, error)
}

// WaitReady waits for a complete idle folder and its expected paired device.
func WaitReady(ctx context.Context, source StatusSource, folderID, pairedDeviceID string, interval time.Duration) error {
	if source == nil {
		return errors.New("Syncthing status source is unavailable")
	}
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}

	lastStatus := FolderStatus{State: "unknown"}
	lastConnected := false
	for {
		status, err := source.FolderStatus(ctx, folderID)
		if err != nil {
			return fmt.Errorf("read Syncthing folder %s status: %w", folderID, err)
		}
		connections, err := source.Connections(ctx)
		if err != nil {
			return fmt.Errorf("read Syncthing device connections: %w", err)
		}
		lastStatus = status
		lastConnected = connections[pairedDeviceID].Connected
		if status.State == "idle" && status.NeedTotalItems == 0 && status.PullErrors == 0 && lastConnected {
			return nil
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf(
				"Syncthing folder %s not ready: state=%s need=%d pull_errors=%d connected=%t: %w",
				folderID, lastStatus.State, lastStatus.NeedTotalItems, lastStatus.PullErrors, lastConnected, ctx.Err(),
			)
		case <-timer.C:
		}
	}
}
