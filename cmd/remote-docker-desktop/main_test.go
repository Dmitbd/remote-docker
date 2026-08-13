package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/desktop"
	"github.com/Dmitbd/remote-docker/internal/lifecycle"
	"github.com/Dmitbd/remote-docker/internal/localapi"
)

func TestDesktopAPIShowWindowReportsConfirmedResult(t *testing.T) {
	handler := &desktopAPIHandler{}
	handler.setShow(func() error { return nil })
	result, err := handler.Handle(context.Background(), localapi.MethodShowWindow, nil)
	if err != nil {
		t.Fatalf("Handle(ShowWindow) error = %v", err)
	}
	if got := result.(map[string]bool)["shown"]; !got {
		t.Fatalf("Handle(ShowWindow) shown = %t, want true", got)
	}
}

func TestDesktopAPIShowWindowRequiresRegisteredApplication(t *testing.T) {
	handler := &desktopAPIHandler{}
	result, err := handler.Handle(context.Background(), localapi.MethodShowWindow, nil)
	if err == nil {
		t.Fatal("Handle(ShowWindow) error = nil without application")
	}
	if result != nil {
		t.Fatalf("Handle(ShowWindow) result = %#v, want nil", result)
	}
}

func TestDesktopAPIShowWindowPropagatesApplicationError(t *testing.T) {
	wantErr := errors.New("focus failed")
	handler := &desktopAPIHandler{}
	handler.setShow(func() error { return wantErr })
	result, err := handler.Handle(context.Background(), localapi.MethodShowWindow, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Handle(ShowWindow) error = %v, want %v", err, wantErr)
	}
	if result != nil {
		t.Fatalf("Handle(ShowWindow) result = %#v, want nil", result)
	}

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	done := make(chan error, 1)
	go func() {
		done <- (localapi.Server{
			Handler:       handler,
			AuthorizePeer: func(net.Conn) error { return nil },
		}).ServeConn(context.Background(), serverConn)
	}()
	client := localapi.Client{Dial: func(context.Context) (net.Conn, error) { return clientConn, nil }}
	var shown struct {
		Shown bool `json:"shown"`
	}
	err = client.Call(context.Background(), localapi.MethodShowWindow, nil, &shown)
	var remoteErr *localapi.RemoteError
	if !errors.As(err, &remoteErr) || remoteErr.Code != localapi.ErrorInternal {
		t.Fatalf("local API error = %v, want public internal boundary", err)
	}
	if shown.Shown {
		t.Fatal("local API returned shown:true after focus error")
	}
	if err := <-done; err != nil {
		t.Fatalf("ServeConn() error = %v", err)
	}
}

func TestShowExistingDesktopRequiresConfirmedPayload(t *testing.T) {
	tests := []struct {
		name    string
		result  showWindowResult
		callErr error
	}{
		{name: "call error", callErr: errors.New("transport failed")},
		{name: "shown false", result: showWindowResult{Shown: false}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := showExistingDesktop(context.Background(), time.Second, func(_ context.Context, method localapi.Method, _ any, result any) error {
				if method != localapi.MethodShowWindow {
					t.Fatalf("method = %q, want %q", method, localapi.MethodShowWindow)
				}
				payload, marshalErr := json.Marshal(test.result)
				if marshalErr != nil {
					t.Fatalf("marshal result: %v", marshalErr)
				}
				if unmarshalErr := json.Unmarshal(payload, result); unmarshalErr != nil {
					t.Fatalf("unmarshal result: %v", unmarshalErr)
				}
				return test.callErr
			})
			if err == nil {
				t.Fatal("showExistingDesktop() error = nil, want failure")
			}
		})
	}
}

func TestShowExistingDesktopAcceptsConfirmedPayload(t *testing.T) {
	err := showExistingDesktop(context.Background(), time.Second, func(_ context.Context, _ localapi.Method, _ any, result any) error {
		result.(*showWindowResult).Shown = true
		return nil
	})
	if err != nil {
		t.Fatalf("showExistingDesktop() error = %v", err)
	}
}

func TestShowExistingDesktopUsesBoundedContext(t *testing.T) {
	started := time.Now()
	err := showExistingDesktop(context.Background(), 10*time.Millisecond, func(ctx context.Context, _ localapi.Method, _ any, _ any) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("showExistingDesktop() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("showExistingDesktop() elapsed = %v, want bounded call", elapsed)
	}
}

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

func TestDesktopUpgradeGateStopsLegacyWriterBeforePersistingV6(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":5}`), 0o600); err != nil {
		t.Fatalf("seed schema v5 config: %v", err)
	}
	events := []string{}
	lock := &recordingInstanceLock{onClose: func() { events = append(events, "unlock") }}
	got, err := acquireDesktopUpgradeGate(context.Background(), "windows", path, desktopUpgradeDependencies{
		acquireInstance: func(string) (desktop.InstanceLock, error) {
			events = append(events, "lock")
			return lock, nil
		},
		stopLegacy: func(context.Context) error {
			events = append(events, "stop-legacy")
			return nil
		},
		confirmLegacyStopped: func(context.Context) error {
			events = append(events, "confirm-stopped")
			return nil
		},
		upgradeConfig: func(context.Context, string) error {
			events = append(events, "write-v6")
			return nil
		},
	})
	if err != nil || got != lock {
		t.Fatalf("acquireDesktopUpgradeGate() lock=%#v error=%v", got, err)
	}
	want := []string{"lock", "stop-legacy", "confirm-stopped", "write-v6"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("upgrade gate order = %v, want %v", events, want)
	}
	_ = got.Close()
}

func TestDesktopUpgradeGateRejectsPipeAbsenceWhileLegacyProcessExists(t *testing.T) {
	wantErr := errors.New("legacy process still starting")
	upgraded := false
	_, err := acquireDesktopUpgradeGate(context.Background(), "windows", filepath.Join(t.TempDir(), "config.json"), desktopUpgradeDependencies{
		acquireInstance: func(string) (desktop.InstanceLock, error) { return &recordingInstanceLock{}, nil },
		stopLegacy:      func(context.Context) error { return nil },
		confirmLegacyStopped: func(context.Context) error {
			return wantErr
		},
		upgradeConfig: func(context.Context, string) error {
			upgraded = true
			return nil
		},
	})
	if !errors.Is(err, wantErr) || upgraded {
		t.Fatalf("upgrade gate error=%v upgraded=%t, want process proof failure", err, upgraded)
	}
}

func TestDesktopUpgradeGateReleasesInstanceWhenLegacyStopFails(t *testing.T) {
	wantErr := errors.New("legacy process did not stop")
	closed := false
	_, err := acquireDesktopUpgradeGate(context.Background(), "windows", filepath.Join(t.TempDir(), "config.json"), desktopUpgradeDependencies{
		acquireInstance: func(string) (desktop.InstanceLock, error) {
			return &recordingInstanceLock{onClose: func() { closed = true }}, nil
		},
		stopLegacy:           func(context.Context) error { return wantErr },
		confirmLegacyStopped: func(context.Context) error { return nil },
		upgradeConfig:        func(context.Context, string) error { t.Fatal("config upgraded before legacy stop"); return nil },
	})
	if !errors.Is(err, wantErr) || !closed {
		t.Fatalf("upgrade gate error=%v closed=%t", err, closed)
	}
}

func TestStopLegacyWindowsDesktopWaitsUntilLocalOwnerExits(t *testing.T) {
	calls := []localapi.Method{}
	statusCalls := 0
	err := stopLegacyWindowsDesktop(context.Background(), time.Millisecond, func(_ context.Context, method localapi.Method) error {
		calls = append(calls, method)
		if method == localapi.MethodStatus {
			statusCalls++
			if statusCalls >= 3 {
				return os.ErrNotExist
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stopLegacyWindowsDesktop() error = %v", err)
	}
	want := []localapi.Method{
		localapi.MethodStatus,
		localapi.MethodShutdown, localapi.MethodStatus,
		localapi.MethodShutdown, localapi.MethodStatus,
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("legacy shutdown calls = %v, want %v", calls, want)
	}
}

func TestStopLegacyWindowsDesktopFailsClosedOnAmbiguousStatusError(t *testing.T) {
	wantErr := context.DeadlineExceeded
	err := stopLegacyWindowsDesktop(context.Background(), time.Millisecond, func(context.Context, localapi.Method) error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("ambiguous status error = %v, want fail-closed %v", err, wantErr)
	}
}

type recordingInstanceLock struct{ onClose func() }

func (l *recordingInstanceLock) Close() error {
	if l.onClose != nil {
		l.onClose()
	}
	return nil
}

func TestInitialTrustedPeerRestoresOnlyActivePublicRecord(t *testing.T) {
	store := config.Store{Path: filepath.Join(t.TempDir(), "config.json")}
	if peer := initialTrustedPeer(store, lifecycle.RoleMacClient); peer != nil {
		t.Fatalf("missing config peer = %#v", peer)
	}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion, ActiveDevice: "windows",
		Devices: map[string]config.Device{"windows": {
			Name: "Windows PC", Address: "192.168.1.20", PairingGeneration: "generation-one",
		}},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	peer := initialTrustedPeer(store, lifecycle.RoleMacClient)
	if peer == nil || peer.ID != "windows" || peer.Name != "Windows PC" || peer.OS != "windows" || peer.Address != "192.168.1.20" ||
		peer.Generation != "generation-one" {
		t.Fatalf("restored peer = %#v", peer)
	}
}

func TestInitialTrustedPeerRejectsWindowsTrustWithoutGeneration(t *testing.T) {
	store := config.Store{Path: filepath.Join(t.TempDir(), "config.json")}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion, ActiveDevice: "mac-one",
		Devices: map[string]config.Device{"mac-one": {Name: "Mac"}},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if peer := initialTrustedPeer(store, lifecycle.RoleWindowsHost); peer != nil {
		t.Fatalf("Windows restored empty-generation trust = %#v", peer)
	}
	peer := initialTrustedPeer(store, lifecycle.RoleMacClient)
	if peer == nil || peer.ID != "mac-one" || peer.Name != "Mac" || peer.Generation != "" {
		t.Fatalf("Mac restored peer = %#v", peer)
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
