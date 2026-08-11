//go:build !windows

package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/config"
)

func TestStateLockSerializesProcesses(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, "state.lock")
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

func TestStateLockHelper(t *testing.T) {
	if os.Getenv("REMOTE_DOCKER_LOCK_HELPER") != "1" {
		t.Skip("helper process")
	}
	lockPath := os.Getenv("REMOTE_DOCKER_LOCK_PATH")
	readyPath := os.Getenv("REMOTE_DOCKER_LOCK_READY")
	releasePath := os.Getenv("REMOTE_DOCKER_LOCK_RELEASE")
	locker := newStateLocker(lockPath)
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

func TestCompletionLeaseCoordinatesIndependentProcesses(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	generation := "pairing-generation"
	now := time.Now().UTC()
	store := config.Store{Path: path}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		PendingRevocations: map[string]config.PendingRevocation{generation: {
			Generation: generation, SessionID: "pairing-session",
			CompletionLeaseToken:     "first-process-token",
			CompletionLeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano),
			Device:                   config.Device{ClientDeviceID: "LOCAL-SYNC"},
		}},
	}); err != nil {
		t.Fatalf("seed completion lease: %v", err)
	}

	live := completionLeaseHelperCommand(t, path, generation, now, false)
	if output, err := live.CombinedOutput(); err != nil {
		t.Fatalf("second process touched live completion lease: %v\n%s", err, output)
	}
	cfg, err := store.Load()
	if err != nil || cfg.PendingRevocations[generation].CleanupRequested {
		t.Fatalf("live lease after second process = %#v error=%v", cfg.PendingRevocations[generation], err)
	}

	afterCrash := completionLeaseHelperCommand(t, path, generation, now.Add(2*time.Minute), true)
	if output, err := afterCrash.CombinedOutput(); err != nil {
		t.Fatalf("second process did not recover expired completion lease: %v\n%s", err, output)
	}
	cfg, err = store.Load()
	if err != nil || !cfg.PendingRevocations[generation].CleanupRequested {
		t.Fatalf("expired lease after recovery = %#v error=%v", cfg.PendingRevocations[generation], err)
	}
}

func TestCompletionLeaseProcessHelper(t *testing.T) {
	if os.Getenv("REMOTE_DOCKER_COMPLETION_LEASE_HELPER") != "1" {
		t.Skip("helper process")
	}
	now, err := time.Parse(time.RFC3339Nano, os.Getenv("REMOTE_DOCKER_COMPLETION_LEASE_NOW"))
	if err != nil {
		t.Fatalf("parse helper clock: %v", err)
	}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: config.Store{Path: os.Getenv("REMOTE_DOCKER_COMPLETION_LEASE_CONFIG")},
		Now:   func() time.Time { return now },
	})
	active, err := coordinator.activatePendingCleanupIfAbandoned(
		context.Background(), os.Getenv("REMOTE_DOCKER_COMPLETION_LEASE_GENERATION"),
	)
	if err != nil {
		t.Fatalf("activate completion cleanup: %v", err)
	}
	wantActive := os.Getenv("REMOTE_DOCKER_COMPLETION_LEASE_ACTIVE") == "1"
	if active != wantActive {
		t.Fatalf("cleanup active=%t, want %t", active, wantActive)
	}
}

func dockerContextLockHelperCommand(t *testing.T, lockPath, readyPath, releasePath string) *exec.Cmd {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestStateLockHelper$", "-test.v")
	command.Env = append(os.Environ(),
		"REMOTE_DOCKER_LOCK_HELPER=1",
		"REMOTE_DOCKER_LOCK_PATH="+lockPath,
		"REMOTE_DOCKER_LOCK_READY="+readyPath,
		"REMOTE_DOCKER_LOCK_RELEASE="+releasePath,
	)
	return command
}

func completionLeaseHelperCommand(t *testing.T, configPath, generation string, now time.Time, wantActive bool) *exec.Cmd {
	t.Helper()
	active := "0"
	if wantActive {
		active = "1"
	}
	command := exec.Command(os.Args[0], "-test.run=^TestCompletionLeaseProcessHelper$", "-test.v")
	command.Env = append(os.Environ(),
		"REMOTE_DOCKER_COMPLETION_LEASE_HELPER=1",
		"REMOTE_DOCKER_COMPLETION_LEASE_CONFIG="+configPath,
		"REMOTE_DOCKER_COMPLETION_LEASE_GENERATION="+generation,
		"REMOTE_DOCKER_COMPLETION_LEASE_NOW="+now.Format(time.RFC3339Nano),
		"REMOTE_DOCKER_COMPLETION_LEASE_ACTIVE="+active,
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
