package desktop

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitForNoOtherInstanceWaitsForProcessExit(t *testing.T) {
	checks := 0
	err := waitForNoOtherInstance(context.Background(), time.Millisecond, func() (bool, error) {
		checks++
		return checks < 3, nil
	})
	if err != nil || checks != 3 {
		t.Fatalf("waitForNoOtherInstance() checks=%d error=%v", checks, err)
	}
}

func TestWaitForNoOtherInstanceFailsClosedOnScanError(t *testing.T) {
	wantErr := errors.New("process scan unavailable")
	err := waitForNoOtherInstance(context.Background(), time.Millisecond, func() (bool, error) {
		return false, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("waitForNoOtherInstance() error=%v, want %v", err, wantErr)
	}
}
