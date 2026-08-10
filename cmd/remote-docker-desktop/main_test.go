package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/lifecycle"
)

func TestWaitForDesktopShutdownRequiresCompletion(t *testing.T) {
	done := make(chan error)
	if err := waitForDesktopShutdown(done, time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForDesktopShutdown() error = %v, want deadline exceeded", err)
	}
	completed := make(chan error, 1)
	completed <- nil
	if err := waitForDesktopShutdown(completed, time.Second); err != nil {
		t.Fatalf("completed waitForDesktopShutdown() error = %v", err)
	}
	wantErr := errors.New("runtime stop failed")
	failed := make(chan error, 1)
	failed <- wantErr
	if err := waitForDesktopShutdown(failed, time.Second); !errors.Is(err, wantErr) {
		t.Fatalf("failed waitForDesktopShutdown() error = %v, want %v", err, wantErr)
	}
}

func TestInitialTrustedPeerRestoresOnlyActivePublicRecord(t *testing.T) {
	store := config.Store{Path: filepath.Join(t.TempDir(), "config.json")}
	if peer := initialTrustedPeer(store, lifecycle.RoleMacClient); peer != nil {
		t.Fatalf("missing config peer = %#v", peer)
	}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion, ActiveDevice: "windows",
		Devices: map[string]config.Device{"windows": {Name: "Windows PC", Address: "192.168.1.20"}},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	peer := initialTrustedPeer(store, lifecycle.RoleMacClient)
	if peer == nil || peer.ID != "windows" || peer.Name != "Windows PC" || peer.OS != "windows" || peer.Address != "192.168.1.20" {
		t.Fatalf("restored peer = %#v", peer)
	}
}

func TestUIExecutableLivesBesideDesktopHost(t *testing.T) {
	desktopPath := filepath.Join(string(filepath.Separator), "Applications", "Remote Docker.app", "Contents", "MacOS", "remote-docker-desktop")
	got := uiExecutablePath(desktopPath)
	if filepath.Dir(got) != filepath.Dir(desktopPath) || filepath.Base(got) == filepath.Base(desktopPath) {
		t.Fatalf("uiExecutablePath() = %q", got)
	}
}

func TestConfigureDesktopShellUsesAccessoryPolicyOnlyOnDarwin(t *testing.T) {
	calls := 0
	setAccessory := func() error {
		calls++
		return nil
	}
	if err := configureDesktopShell("darwin", setAccessory); err != nil || calls != 1 {
		t.Fatalf("darwin shell configuration calls=%d error=%v", calls, err)
	}
	calls = 0
	if err := configureDesktopShell("windows", setAccessory); err != nil || calls != 0 {
		t.Fatalf("windows shell configuration calls=%d error=%v", calls, err)
	}
	wantErr := errors.New("accessory policy unavailable")
	if err := configureDesktopShell("darwin", func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("darwin shell configuration error=%v, want %v", err, wantErr)
	}
}

func TestCompleteDesktopShutdownPreservesOwnedCleanupOrder(t *testing.T) {
	events := []string{}
	err := completeDesktopShutdown(
		func(context.Context) error {
			events = append(events,
				"notify-peer", "stop-internal-streams", "stop-sync", "stop-tunnel",
				"stop-managed-wsl", "stop-owned-children", "stop-watchdog",
			)
			return nil
		},
		func() error {
			events = append(events, "close-local-api")
			return nil
		},
		func(context.Context) error {
			events = append(events, "stop-ui-and-tray")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("completeDesktopShutdown() error = %v", err)
	}
	want := []string{
		"notify-peer", "stop-internal-streams", "stop-sync", "stop-tunnel",
		"stop-managed-wsl", "stop-owned-children", "stop-watchdog",
		"close-local-api", "stop-ui-and-tray",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("shutdown order = %v, want %v", events, want)
	}
}
