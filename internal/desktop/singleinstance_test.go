package desktop

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestSingleInstanceLockIsReleasedOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote-docker.lock")
	first, err := AcquireSingleInstance(path)
	if err != nil {
		t.Fatalf("AcquireSingleInstance(first) error = %v", err)
	}
	if _, err := AcquireSingleInstance(path); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("AcquireSingleInstance(second) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	third, err := AcquireSingleInstance(path)
	if err != nil {
		t.Fatalf("AcquireSingleInstance(after close) error = %v", err)
	}
	_ = third.Close()
}
