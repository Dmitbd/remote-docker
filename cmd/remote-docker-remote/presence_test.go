package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPresenceManagerOwnsOneLeaseAndRejectsSequenceReplay(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	manager := newPresenceManager(presenceOptions{
		Now: func() time.Time { return now },
		Random: strings.NewReader("0123456789abcdef0123456789abcdef"),
		Ready: func(context.Context) (bool, bool) { return true, true },
	})
	hello, err := manager.Hello(context.Background(), presenceHelloParams{
		ClientDeviceID: "mac-one", ClientName: "MacBook", AppVersion: "0.1.0",
	})
	if err != nil || hello.SessionID == "" || !hello.DockerReady || !hello.SyncReady {
		t.Fatalf("Hello() = %#v, %v", hello, err)
	}
	if _, err := manager.Hello(context.Background(), presenceHelloParams{
		ClientDeviceID: "mac-two", ClientName: "Other Mac", AppVersion: "0.1.0",
	}); err == nil {
		t.Fatal("Hello() accepted a second live client")
	}
	first, err := manager.Heartbeat(context.Background(), presenceHeartbeatParams{SessionID: hello.SessionID, Sequence: 1})
	if err != nil || first.Terminal {
		t.Fatalf("Heartbeat(1) = %#v, %v", first, err)
	}
	if _, err := manager.Heartbeat(context.Background(), presenceHeartbeatParams{SessionID: hello.SessionID, Sequence: 1}); err == nil {
		t.Fatal("Heartbeat() accepted a replayed sequence")
	}
}

func TestPresenceManagerSurfacesExplicitDisconnectWithoutLeakingIdentity(t *testing.T) {
	manager := newPresenceManager(presenceOptions{Random: strings.NewReader("0123456789abcdef0123456789abcdef")})
	hello, err := manager.Hello(context.Background(), presenceHelloParams{
		ClientDeviceID: "sensitive-device-id", ClientName: "MacBook", AppVersion: "0.1.0",
	})
	if err != nil {
		t.Fatalf("Hello() error = %v", err)
	}
	if err := manager.RequestTerminal(hello.SessionID, "windows_disconnect"); err != nil {
		t.Fatalf("RequestTerminal() error = %v", err)
	}
	heartbeat, err := manager.Heartbeat(context.Background(), presenceHeartbeatParams{SessionID: hello.SessionID, Sequence: 1})
	if err != nil || !heartbeat.Terminal || heartbeat.Reason != "windows_disconnect" {
		t.Fatalf("terminal heartbeat = %#v, %v", heartbeat, err)
	}
	if _, err := manager.Heartbeat(context.Background(), presenceHeartbeatParams{SessionID: hello.SessionID, Sequence: 2}); err == nil ||
		strings.Contains(err.Error(), "sensitive-device-id") || strings.Contains(err.Error(), hello.SessionID) {
		t.Fatalf("post-terminal error leaked identity: %v", err)
	}
}

func TestPresenceManagerMacDisconnectClosesLease(t *testing.T) {
	manager := newPresenceManager(presenceOptions{Random: strings.NewReader("0123456789abcdef0123456789abcdef")})
	hello, _ := manager.Hello(context.Background(), presenceHelloParams{
		ClientDeviceID: "mac", ClientName: "Mac", AppVersion: "0.1.0",
	})
	if err := manager.Disconnect(context.Background(), presenceDisconnectParams{
		SessionID: hello.SessionID, Reason: "user_disconnect",
	}); err != nil {
		t.Fatalf("Disconnect() error = %v", err)
	}
	if _, err := manager.Heartbeat(context.Background(), presenceHeartbeatParams{SessionID: hello.SessionID, Sequence: 1}); !errors.Is(err, errPresenceSession) {
		t.Fatalf("Heartbeat() error = %v, want session error", err)
	}
}
