//go:build !windows

package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDockerContextLockSerializesProcesses(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, "docker-context.lock")
	firstReady := filepath.Join(root, "first-ready")
	releaseFirst := filepath.Join(root, "release-first")
	secondReady := filepath.Join(root, "second-ready")

	first := dockerContextLockHelperCommand(t, lockPath, firstReady, releaseFirst)
	if err := first.Start(); err != nil {
		t.Fatalf("start first helper: %v", err)
	}
	waitForLockMarker(t, firstReady)

	second := dockerContextLockHelperCommand(t, lockPath, secondReady, "")
	if err := second.Start(); err != nil {
		t.Fatalf("start second helper: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(secondReady); err == nil {
		t.Fatal("second process entered the Docker context transaction before the first released it")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat second marker: %v", err)
	}
	if err := os.WriteFile(releaseFirst, []byte("release"), 0o600); err != nil {
		t.Fatalf("release first helper: %v", err)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("first helper: %v", err)
	}
	if err := second.Wait(); err != nil {
		t.Fatalf("second helper: %v", err)
	}
	waitForLockMarker(t, secondReady)
}

func TestDockerContextLockHelper(t *testing.T) {
	if os.Getenv("REMOTE_DOCKER_LOCK_HELPER") != "1" {
		t.Skip("helper process")
	}
	lockPath := os.Getenv("REMOTE_DOCKER_LOCK_PATH")
	readyPath := os.Getenv("REMOTE_DOCKER_LOCK_READY")
	releasePath := os.Getenv("REMOTE_DOCKER_LOCK_RELEASE")
	locker := newDockerContextLocker(lockPath)
	if err := locker.WithLock(context.Background(), func() error {
		if err := os.WriteFile(readyPath, []byte("ready"), 0o600); err != nil {
			return err
		}
		for releasePath != "" {
			if _, err := os.Stat(releasePath); err == nil {
				break
			} else if !os.IsNotExist(err) {
				return err
			}
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithLock() error = %v", err)
	}
}

func dockerContextLockHelperCommand(t *testing.T, lockPath, readyPath, releasePath string) *exec.Cmd {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestDockerContextLockHelper$", "-test.v")
	command.Env = append(os.Environ(),
		"REMOTE_DOCKER_LOCK_HELPER=1",
		"REMOTE_DOCKER_LOCK_PATH="+lockPath,
		"REMOTE_DOCKER_LOCK_READY="+readyPath,
		"REMOTE_DOCKER_LOCK_RELEASE="+releasePath,
	)
	return command
}

func waitForLockMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat lock marker: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for lock marker %s", path)
}
