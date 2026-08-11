package filelock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestFileLockSerializesAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.lock")
	first, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire(first) error = %v", err)
	}
	if _, err := TryAcquire(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("TryAcquire(second) error = %v, want ErrLocked", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire(blocked) error = %v, want deadline", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}
	afterClose, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire(after close) error = %v", err)
	}
	_ = afterClose.Close()
}
