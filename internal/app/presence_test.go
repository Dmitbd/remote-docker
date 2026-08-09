package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
)

func TestHostPresenceRecoversBeforeDeadlineWithoutCleanup(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	machine := trustedLifecycleMachine(t, lifecycle.RoleWindowsHost, &now)
	cleanups := 0
	presence, err := NewHostPresence(machine, func() time.Time { return now }, func(context.Context) error {
		cleanups++
		return nil
	})
	if err != nil {
		t.Fatalf("NewHostPresence() error = %v", err)
	}
	if err := presence.Hello("session", lifecycle.Peer{ID: "mac", Name: "Mac"}); err != nil {
		t.Fatalf("Hello() error = %v", err)
	}
	if err := presence.Heartbeat("session", 1, 12*time.Millisecond); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	now = now.Add(presenceHeartbeatTimeout + time.Second)
	if err := presence.Tick(context.Background()); err != nil {
		t.Fatalf("Tick(network loss) error = %v", err)
	}
	if got := machine.Snapshot(); got.State != lifecycle.StateReconnecting || got.Recovery == nil {
		t.Fatalf("reconnecting snapshot = %#v", got)
	}
	now = now.Add(20 * time.Second)
	if err := presence.Heartbeat("session", 2, 9*time.Millisecond); err != nil {
		t.Fatalf("Heartbeat(recovered) error = %v", err)
	}
	if got := machine.Snapshot(); got.State != lifecycle.StateConnected || got.Recovery != nil || got.Latency != 9*time.Millisecond {
		t.Fatalf("recovered snapshot = %#v", got)
	}
	if cleanups != 0 {
		t.Fatalf("cleanup calls = %d", cleanups)
	}
}

func TestHostPresenceNetworkTimeoutCleansUpExactlyOnce(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	machine := trustedLifecycleMachine(t, lifecycle.RoleWindowsHost, &now)
	cleanups := 0
	presence, _ := NewHostPresence(machine, func() time.Time { return now }, func(context.Context) error {
		cleanups++
		return nil
	})
	_ = presence.Hello("session", lifecycle.Peer{ID: "mac", Name: "Mac"})
	_ = presence.Heartbeat("session", 1, time.Millisecond)
	now = now.Add(presenceHeartbeatTimeout + time.Second)
	_ = presence.Tick(context.Background())
	now = now.Add(60 * time.Second)
	if err := presence.Tick(context.Background()); err != nil {
		t.Fatalf("Tick(timeout) error = %v", err)
	}
	if err := presence.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick() error = %v", err)
	}
	if cleanups != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanups)
	}
	got := machine.Snapshot()
	if got.State != lifecycle.StateHostWaiting || got.LastDisconnect == nil || got.LastDisconnect.Reason != lifecycle.ReasonNetworkTimeout {
		t.Fatalf("timeout snapshot = %#v", got)
	}
}

func TestHostPresenceRejectsReplayAndSecondLease(t *testing.T) {
	now := time.Now()
	machine := trustedLifecycleMachine(t, lifecycle.RoleWindowsHost, &now)
	presence, _ := NewHostPresence(machine, func() time.Time { return now }, func(context.Context) error { return nil })
	if err := presence.Hello("session-one", lifecycle.Peer{ID: "mac", Name: "Mac"}); err != nil {
		t.Fatalf("Hello() error = %v", err)
	}
	if err := presence.Hello("session-two", lifecycle.Peer{ID: "other", Name: "Other"}); err == nil {
		t.Fatal("second lease was accepted")
	}
	if err := presence.Heartbeat("session-one", 2, 0); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	if err := presence.Heartbeat("session-one", 2, 0); !errors.Is(err, ErrPresenceSequence) {
		t.Fatalf("replayed heartbeat error = %v", err)
	}
}

func TestClientPresenceReportsLocalAndWindowsDisconnectInitiators(t *testing.T) {
	t.Run("Mac disconnects", func(t *testing.T) {
		now := time.Now()
		machine := trustedLifecycleMachine(t, lifecycle.RoleMacClient, &now)
		transport := &recordingPresenceTransport{sessionID: "session"}
		cleanups := 0
		presence, _ := NewClientPresence(machine, transport, func() time.Time { return now }, func(context.Context) error {
			cleanups++
			return nil
		})
		if err := presence.Start(context.Background(), "mac", "MacBook", "0.1.0"); err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		if err := presence.Disconnect(context.Background()); err != nil {
			t.Fatalf("Disconnect() error = %v", err)
		}
		got := machine.Snapshot()
		if transport.disconnectReason != string(lifecycle.ReasonUserDisconnect) || cleanups != 1 || got.State != lifecycle.StateClientReady ||
			got.LastDisconnect == nil || got.LastDisconnect.Initiator != lifecycle.InitiatorLocal {
			t.Fatalf("local disconnect transport=%q cleanups=%d snapshot=%#v", transport.disconnectReason, cleanups, got)
		}
	})

	t.Run("Windows disconnects", func(t *testing.T) {
		now := time.Now()
		machine := trustedLifecycleMachine(t, lifecycle.RoleMacClient, &now)
		transport := &recordingPresenceTransport{
			sessionID: "session", heartbeat: PresenceHeartbeatResult{Terminal: true, Reason: "windows_disconnect"},
		}
		presence, _ := NewClientPresence(machine, transport, func() time.Time { return now }, func(context.Context) error { return nil })
		_ = presence.Start(context.Background(), "mac", "MacBook", "0.1.0")
		if err := presence.Heartbeat(context.Background()); err != nil {
			t.Fatalf("Heartbeat() error = %v", err)
		}
		got := machine.Snapshot()
		if got.State != lifecycle.StateClientReady || got.LastDisconnect == nil || got.LastDisconnect.Initiator != lifecycle.InitiatorPeer {
			t.Fatalf("peer disconnect snapshot = %#v", got)
		}
	})
}

type recordingPresenceTransport struct {
	sessionID        string
	heartbeat        PresenceHeartbeatResult
	disconnectReason string
}

func (t *recordingPresenceTransport) Hello(context.Context, PresenceHello) (PresenceHelloResult, error) {
	return PresenceHelloResult{SessionID: t.sessionID, DockerReady: true, SyncReady: true}, nil
}

func (t *recordingPresenceTransport) Heartbeat(context.Context, string, uint64) (PresenceHeartbeatResult, error) {
	return t.heartbeat, nil
}

func (t *recordingPresenceTransport) Disconnect(_ context.Context, _ string, reason string) error {
	t.disconnectReason = reason
	return nil
}

func trustedLifecycleMachine(t *testing.T, role lifecycle.Role, now *time.Time) *lifecycle.Machine {
	t.Helper()
	machine, err := lifecycle.NewMachine(role, "Device", lifecycle.WithClock(func() time.Time { return *now }),
		lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "mac", Name: "Mac"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled}); err != nil {
		t.Fatalf("enable error = %v", err)
	}
	return machine
}
