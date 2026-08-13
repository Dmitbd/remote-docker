package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
	"github.com/Dmitbd/remote-docker/internal/windowsbridge"
)

func TestClientConnectionRuntimeRetriesSyncBootstrapBeforePresence(t *testing.T) {
	now := time.Now()
	machine := trustedLifecycleMachine(t, lifecycle.RoleMacClient, &now)
	transport := &recordingPresenceTransport{sessionID: "session"}
	prepares := 0
	starts := 0
	runtime := &clientConnectionRuntime{
		machine: machine, clientDeviceID: func() string { return "mac-sync" },
		prepare: func(context.Context) error {
			prepares++
			if prepares == 1 {
				return errors.New("sync peer unavailable")
			}
			return nil
		},
		ready:     func() bool { return true },
		localName: "MacBook", appVersion: "0.2.9",
		transport: func(context.Context) (PresenceTransport, error) {
			starts++
			return transport, nil
		},
	}

	if err := runtime.step(context.Background()); err == nil {
		t.Fatal("first step succeeded before Syncthing peer bootstrap")
	}
	if starts != 0 {
		t.Fatalf("transport starts before bootstrap = %d, want 0", starts)
	}
	if err := runtime.step(context.Background()); err != nil {
		t.Fatalf("second step error = %v", err)
	}
	if starts != 1 || prepares != 2 {
		t.Fatalf("after bootstrap starts=%d prepares=%d, want 1/2", starts, prepares)
	}
	if err := runtime.step(context.Background()); err != nil {
		t.Fatalf("third step error = %v", err)
	}
	if prepares != 2 {
		t.Fatalf("successful bootstrap repeated: prepares=%d, want 2", prepares)
	}
}

func TestClientConnectionRuntimeCreatesOneAuthenticatedLeaseAndDrivesReadiness(t *testing.T) {
	now := time.Now()
	machine := trustedLifecycleMachine(t, lifecycle.RoleMacClient, &now)
	transport := &recordingPresenceTransport{
		sessionID:  "session",
		hello:      PresenceHelloResult{SessionID: "session", DockerReady: false, SyncReady: true},
		heartbeats: []PresenceHeartbeatResult{{DockerReady: true, SyncReady: true}},
	}
	starts := 0
	runtime := &clientConnectionRuntime{
		machine: machine, clientDeviceID: func() string { return "mac-sync" },
		ready:     func() bool { return true },
		localName: "MacBook", appVersion: "0.2.5",
		transport: func(context.Context) (PresenceTransport, error) { starts++; return transport, nil },
	}
	if err := runtime.step(context.Background()); err != nil {
		t.Fatalf("first step error = %v", err)
	}
	if got := machine.Snapshot(); got.State != lifecycle.StateConnecting {
		t.Fatalf("first snapshot = %#v", got)
	}
	if err := runtime.step(context.Background()); err != nil {
		t.Fatalf("heartbeat step error = %v", err)
	}
	if got := machine.Snapshot(); got.State != lifecycle.StateConnected {
		t.Fatalf("connected snapshot = %#v", got)
	}
	if starts != 1 {
		t.Fatalf("transport starts = %d, want 1", starts)
	}
}

func TestClientConnectionRuntimeNotifiesPeerBeforeSupervisorStopsInfrastructure(t *testing.T) {
	now := time.Now()
	machine := trustedLifecycleMachine(t, lifecycle.RoleMacClient, &now)
	transport := &recordingPresenceTransport{sessionID: "session"}
	runtime := &clientConnectionRuntime{
		machine: machine, clientDeviceID: func() string { return "mac-sync" },
		ready:     func() bool { return true },
		localName: "MacBook", appVersion: "0.2.5",
		transport: func(context.Context) (PresenceTransport, error) { return transport, nil },
	}
	if err := runtime.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Stop(context.Background(), lifecycle.StopPause); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if transport.disconnectReason != string(lifecycle.ReasonUserPause) {
		t.Fatalf("disconnect reason = %q", transport.disconnectReason)
	}
}

func TestHostConnectionRuntimeRequiresPresenceAndBothManagedServices(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	machine := trustedLifecycleMachine(t, lifecycle.RoleWindowsHost, &now)
	observer := &sequenceManagedPresenceObserver{statuses: []windowsbridge.ManagedWSLStatus{
		{Running: true, DockerSocket: true, SyncthingService: true, PresenceActive: false},
		{Running: true, DockerSocket: true, SyncthingService: true, PresenceActive: true},
		{Running: true, DockerSocket: true, SyncthingService: true, PresenceActive: false},
		{Running: true, DockerSocket: true, SyncthingService: true, PresenceActive: true},
	}}
	runtime, err := newHostConnectionRuntime(machine, observer, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.step(context.Background()); err != nil || machine.Snapshot().State != lifecycle.StateHostWaiting {
		t.Fatalf("absent presence step error=%v snapshot=%#v", err, machine.Snapshot())
	}
	if err := runtime.step(context.Background()); err != nil || machine.Snapshot().State != lifecycle.StateConnected {
		t.Fatalf("connected step error=%v snapshot=%#v", err, machine.Snapshot())
	}
	now = now.Add(presenceHeartbeatTimeout + time.Second)
	if err := runtime.step(context.Background()); err != nil || machine.Snapshot().State != lifecycle.StateReconnecting {
		t.Fatalf("lost step error=%v snapshot=%#v", err, machine.Snapshot())
	}
	if err := runtime.step(context.Background()); err != nil || machine.Snapshot().State != lifecycle.StateConnected {
		t.Fatalf("restored step error=%v snapshot=%#v", err, machine.Snapshot())
	}
}

type sequenceManagedPresenceObserver struct {
	statuses []windowsbridge.ManagedWSLStatus
}

func (o *sequenceManagedPresenceObserver) Observe(context.Context) (windowsbridge.ManagedWSLStatus, error) {
	if len(o.statuses) == 0 {
		return windowsbridge.ManagedWSLStatus{}, nil
	}
	status := o.statuses[0]
	o.statuses = o.statuses[1:]
	return status, nil
}
