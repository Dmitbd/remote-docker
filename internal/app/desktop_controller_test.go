package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/pairing"
)

func TestDesktopControllerPublishesPausedLifecycleSnapshot(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	controller, err := NewDesktopController(supervisor, nil)
	if err != nil {
		t.Fatalf("NewDesktopController() error = %v", err)
	}

	result, err := controller.Handle(context.Background(), localapi.MethodStatus, nil)
	if err != nil {
		t.Fatalf("Handle(Status) error = %v", err)
	}
	status, ok := result.(localapi.StatusResult)
	if !ok || status.State != string(lifecycle.StatePaused) || status.Role != string(lifecycle.RoleMacClient) ||
		status.ConnectionLimit != 1 || status.Paired {
		t.Fatalf("status = %#v", result)
	}
}

func TestDesktopControllerSeparatesEnableSearchAndPause(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	runtime := newRecordingSessionRuntime()
	supervisor, _ := NewSupervisor(machine, runtime)
	controller, _ := NewDesktopController(supervisor, nil)

	if _, err := controller.Handle(context.Background(), localapi.MethodEnable, nil); err != nil {
		t.Fatalf("Enable error = %v", err)
	}
	if got := supervisor.Snapshot().State; got != lifecycle.StateClientReady {
		t.Fatalf("state after enable = %q, want client ready", got)
	}
	if _, err := controller.Handle(context.Background(), localapi.MethodSearchStart, nil); err != nil {
		t.Fatalf("SearchStart error = %v", err)
	}
	if got := supervisor.Snapshot().State; got != lifecycle.StateSearching {
		t.Fatalf("state after search start = %q", got)
	}
	if _, err := controller.Handle(context.Background(), localapi.MethodSearchStop, nil); err != nil {
		t.Fatalf("SearchStop error = %v", err)
	}
	if _, err := controller.Handle(context.Background(), localapi.MethodPause, nil); err != nil {
		t.Fatalf("Pause error = %v", err)
	}
	if got := supervisor.Snapshot().State; got != lifecycle.StatePaused {
		t.Fatalf("state after pause = %q", got)
	}
}

func TestDesktopControllerRejectsDockerWhilePausedWithoutAutoStarting(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	runtime := newRecordingSessionRuntime()
	supervisor, _ := NewSupervisor(machine, runtime)
	fallback := &recordingLocalHandler{}
	controller, _ := NewDesktopController(supervisor, fallback)

	_, err := controller.Handle(context.Background(), localapi.MethodPrepareDocker, json.RawMessage(`{"working_directory":"/tmp"}`))
	var public *localapi.PublicError
	if !errors.As(err, &public) || public.Code != localapi.ErrorNeedsAction {
		t.Fatalf("PrepareDocker error = %v, want needs_action", err)
	}
	if runtime.startCalls != 0 || len(fallback.methods) != 0 {
		t.Fatalf("paused Docker auto-started work: starts=%d methods=%v", runtime.startCalls, fallback.methods)
	}
}

func TestDesktopControllerDelegatesExistingOwnerOnlyOperations(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := &recordingLocalHandler{result: localapi.WorkspaceListResult{}}
	controller, _ := NewDesktopController(supervisor, fallback)

	result, err := controller.Handle(context.Background(), localapi.MethodWorkspaceList, nil)
	if err != nil {
		t.Fatalf("WorkspaceList error = %v", err)
	}
	if _, ok := result.(localapi.WorkspaceListResult); !ok || len(fallback.methods) != 1 || fallback.methods[0] != localapi.MethodWorkspaceList {
		t.Fatalf("result=%#v methods=%v", result, fallback.methods)
	}
}

func TestDesktopControllerShutdownWaitsForOwnedRuntime(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleWindowsHost)
	runtime := newRecordingSessionRuntime()
	supervisor, _ := NewSupervisor(machine, runtime)
	controller, _ := NewDesktopController(supervisor, nil)
	if _, err := controller.Handle(context.Background(), localapi.MethodEnable, nil); err != nil {
		t.Fatalf("Enable error = %v", err)
	}

	result, err := controller.Handle(context.Background(), localapi.MethodShutdown, nil)
	if err != nil {
		t.Fatalf("Shutdown error = %v", err)
	}
	shutdown, ok := result.(localapi.ShutdownResult)
	if !ok || !shutdown.Stopped || runtime.stopCalls != 1 || runtime.reason != lifecycle.StopQuit {
		t.Fatalf("shutdown=%#v stopCalls=%d reason=%q", result, runtime.stopCalls, runtime.reason)
	}
}

func TestDesktopControllerStartsDisplayOnlyPairingFromMacSearch(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := &recordingLocalHandler{result: localapi.PairStartResult{
		SessionID: "session-1", Code: "123456", ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
		Peer: localapi.LifecyclePeer{ID: "windows", Name: "Windows PC", OS: "windows"},
	}}
	controller, _ := NewDesktopController(supervisor, fallback)
	_, _ = controller.Handle(context.Background(), localapi.MethodEnable, nil)
	_, _ = controller.Handle(context.Background(), localapi.MethodSearchStart, nil)

	if _, err := controller.Handle(context.Background(), localapi.MethodPairStart, json.RawMessage(`{"device":"windows"}`)); err != nil {
		t.Fatalf("PairStart error = %v", err)
	}
	snapshot := supervisor.Snapshot()
	if snapshot.State != lifecycle.StatePairing || snapshot.Pairing == nil || snapshot.Pairing.Code != "123456" ||
		snapshot.Pairing.Peer.Name != "Windows PC" {
		t.Fatalf("pairing snapshot = %#v", snapshot)
	}
}

func TestDesktopControllerAllowsOnlyOneConcurrentPairStart(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := newBlockingPairStartHandler()
	controller, _ := NewDesktopController(supervisor, fallback)
	_, _ = controller.Handle(context.Background(), localapi.MethodEnable, nil)
	_, _ = controller.Handle(context.Background(), localapi.MethodSearchStart, nil)

	firstDone := make(chan error, 1)
	go func() {
		_, err := controller.Handle(context.Background(), localapi.MethodPairStart, json.RawMessage(`{"device":"windows"}`))
		firstDone <- err
	}()
	waitForTestSignal(t, fallback.started, "first pair bootstrap")

	_, secondErr := controller.Handle(context.Background(), localapi.MethodPairStart, json.RawMessage(`{"device":"windows"}`))
	if secondErr == nil {
		t.Fatal("second concurrent PairStart error = nil")
	}
	if fallback.StartCalls() != 1 {
		t.Fatalf("PairStart delegate calls = %d, want one", fallback.StartCalls())
	}
	close(fallback.release)
	if err := waitForTestError(t, firstDone, "first PairStart completion"); err != nil {
		t.Fatalf("first PairStart error = %v", err)
	}
}

func TestDesktopControllerPairStartExcludesSearchStopAndPause(t *testing.T) {
	for _, method := range []localapi.Method{localapi.MethodSearchStop, localapi.MethodPause} {
		t.Run(string(method), func(t *testing.T) {
			machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
			runtime := newRecordingSessionRuntime()
			supervisor, _ := NewSupervisor(machine, runtime)
			fallback := newBlockingPairStartHandler()
			controller, _ := NewDesktopController(supervisor, fallback)
			_, _ = controller.Handle(context.Background(), localapi.MethodEnable, nil)
			_, _ = controller.Handle(context.Background(), localapi.MethodSearchStart, nil)

			pairDone := make(chan error, 1)
			go func() {
				_, err := controller.Handle(context.Background(), localapi.MethodPairStart, json.RawMessage(`{"device":"windows"}`))
				pairDone <- err
			}()
			waitForTestSignal(t, fallback.started, "pair bootstrap")

			if _, err := controller.Handle(context.Background(), method, nil); err == nil {
				t.Fatalf("concurrent %s error = nil", method)
			}
			if got := supervisor.Snapshot(); got.State != lifecycle.StateSearching || got.Pairing != nil {
				t.Fatalf("snapshot during reserved PairStart = %#v", got)
			}
			if runtime.stopCalls != 0 {
				t.Fatalf("runtime stops during reserved PairStart = %d", runtime.stopCalls)
			}
			close(fallback.release)
			if err := waitForTestError(t, pairDone, "PairStart completion"); err != nil {
				t.Fatalf("PairStart error = %v", err)
			}
		})
	}
}

func TestDesktopControllerCancelsCreatedRemotePairWhenLifecycleCommitFails(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := newBlockingPairStartHandler()
	fallback.release = closedSignal()
	fallback.expiresAt = "invalid-expiry"
	controller, _ := NewDesktopController(supervisor, fallback)
	_, _ = controller.Handle(context.Background(), localapi.MethodEnable, nil)
	_, _ = controller.Handle(context.Background(), localapi.MethodSearchStart, nil)

	_, err := controller.Handle(context.Background(), localapi.MethodPairStart, json.RawMessage(`{"device":"windows"}`))
	if err == nil {
		t.Fatal("PairStart lifecycle commit error = nil")
	}
	if fallback.StartCalls() != 1 || fallback.CancelledSession() != "session-1" {
		t.Fatalf("pair rollback starts=%d cancelled=%q", fallback.StartCalls(), fallback.CancelledSession())
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateSearching || got.Pairing != nil {
		t.Fatalf("snapshot after PairStart rollback = %#v", got)
	}
}

func TestDesktopControllerPairStartRollbackUsesDetachedContext(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := newBlockingPairStartHandler()
	fallback.release = closedSignal()
	fallback.expiresAt = "invalid-expiry"
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	fallback.afterStart = cancelRequest
	controller, _ := NewDesktopController(supervisor, fallback)
	_, _ = controller.Handle(context.Background(), localapi.MethodEnable, nil)
	_, _ = controller.Handle(context.Background(), localapi.MethodSearchStart, nil)

	if _, err := controller.Handle(requestCtx, localapi.MethodPairStart, json.RawMessage(`{"device":"windows"}`)); err == nil {
		t.Fatal("PairStart lifecycle commit error = nil")
	}
	if fallback.CancelContextError() != nil {
		t.Fatalf("PairCancel inherited cancelled request context: %v", fallback.CancelContextError())
	}
}

func TestDesktopControllerKeepsUnconfirmedRollbackVisible(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := newBlockingPairStartHandler()
	fallback.release = closedSignal()
	fallback.expiresAt = "invalid-expiry"
	fallback.cancelErr = errors.New("remote cancel unavailable")
	controller, _ := NewDesktopController(supervisor, fallback)
	_, _ = controller.Handle(context.Background(), localapi.MethodEnable, nil)
	_, _ = controller.Handle(context.Background(), localapi.MethodSearchStart, nil)

	if _, err := controller.Handle(context.Background(), localapi.MethodPairStart, json.RawMessage(`{"device":"windows"}`)); err == nil {
		t.Fatal("PairStart rollback error = nil")
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StatePairing || got.Pairing == nil || got.Pairing.SessionID != "session-1" {
		t.Fatalf("unconfirmed rollback pairing is hidden = %#v", got)
	}
}

func TestDesktopControllerRejectsNewPairBeforeDelegatingWhenLimitIsFull(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := &recordingLocalHandler{}
	controller, _ := NewDesktopController(supervisor, fallback)
	_, _ = controller.Handle(context.Background(), localapi.MethodEnable, nil)
	_, _ = controller.Handle(context.Background(), localapi.MethodSearchStart, nil)

	_, err = controller.Handle(context.Background(), localapi.MethodPairStart, json.RawMessage(`{"device":"new-windows"}`))
	var public *localapi.PublicError
	if !errors.As(err, &public) || public.Code != localapi.ErrorNeedsAction {
		t.Fatalf("PairStart error = %v, want needs_action", err)
	}
	if len(fallback.methods) != 0 {
		t.Fatalf("PairStart delegated methods = %v, want none", fallback.methods)
	}
}

func TestDesktopControllerReplacePreflightPreservesTrustOutsideSearching(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled})
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := &recordingLocalHandler{}
	controller, _ := NewDesktopController(supervisor, fallback)

	_, err = controller.Handle(context.Background(), localapi.MethodReplaceDevice, json.RawMessage(
		`{"old_device_id":"saved","new_device":"new-windows"}`,
	))
	if err == nil {
		t.Fatal("ReplaceDevice outside search error = nil")
	}
	if len(fallback.methods) != 0 {
		t.Fatalf("replacement preflight delegated destructive methods: %v", fallback.methods)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateClientReady || got.TrustedPeers != 1 || got.Peer == nil || got.Peer.ID != "saved" {
		t.Fatalf("trust changed by replacement preflight = %#v", got)
	}
}

func TestDesktopControllerReplaceBootstrapFailurePreservesOldTrust(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled})
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted})
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := &recordingLocalHandler{errors: map[localapi.Method]error{
		localapi.MethodPairStart: unavailable("candidate bootstrap failed"),
	}}
	controller, _ := NewDesktopController(supervisor, fallback)

	if _, err := controller.Handle(context.Background(), localapi.MethodReplaceDevice, json.RawMessage(
		`{"old_device_id":"saved","new_device":"new-windows"}`,
	)); err == nil {
		t.Fatal("ReplaceDevice bootstrap error = nil")
	}
	if !reflect.DeepEqual(fallback.methods, []localapi.Method{localapi.MethodPairStart}) {
		t.Fatalf("bootstrap failure delegated destructive methods: %v", fallback.methods)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateSearching || got.TrustedPeers != 1 || got.Peer == nil || got.Peer.ID != "saved" {
		t.Fatalf("old trust changed after bootstrap failure = %#v", got)
	}
}

func TestDesktopControllerReplaceRevokeFailureCancelsSessionAndPreservesOldTrust(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled})
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted})
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := &recordingLocalHandler{
		results: map[localapi.Method]any{
			localapi.MethodPairStart: localapi.PairStartResult{
				SessionID: "replacement-session", Code: "123456",
				ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
				Peer:      localapi.LifecyclePeer{ID: "new-windows", Name: "New Windows", OS: "windows"},
			},
			localapi.MethodPairCancel: localapi.PairingStatusResult{SessionID: "replacement-session", Status: string(pairing.SessionCancelled)},
		},
		errors: map[localapi.Method]error{
			localapi.MethodUnpair: &localapi.PublicError{Code: localapi.ErrorRemoteRevokeUnavailable, Message: "remote trust revocation is unavailable"},
		},
	}
	controller, _ := NewDesktopController(supervisor, fallback)

	_, err = controller.Handle(context.Background(), localapi.MethodReplaceDevice, json.RawMessage(
		`{"old_device_id":"saved","new_device":"new-windows"}`,
	))
	var public *localapi.PublicError
	if !errors.As(err, &public) || public.Code != localapi.ErrorRemoteRevokeUnavailable {
		t.Fatalf("ReplaceDevice revoke error = %v, want typed remote revoke error", err)
	}
	if !reflect.DeepEqual(fallback.methods, []localapi.Method{localapi.MethodPairStart, localapi.MethodUnpair, localapi.MethodPairCancel}) {
		t.Fatalf("revoke rollback order = %v", fallback.methods)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateSearching || got.TrustedPeers != 1 || got.Peer == nil || got.Peer.ID != "saved" || got.Pairing != nil {
		t.Fatalf("old trust changed after revoke failure = %#v", got)
	}
}

func TestDesktopControllerKeepsFailedReplacementCancellationVisibleAndRetryable(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled})
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted})
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := &recordingLocalHandler{
		results: map[localapi.Method]any{
			localapi.MethodPairStart: localapi.PairStartResult{
				SessionID: "replacement-session", Code: "123456",
				ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
				Peer:      localapi.LifecyclePeer{ID: "new-windows", Name: "New Windows", OS: "windows"},
			},
			localapi.MethodPairCancel: localapi.PairingStatusResult{
				SessionID: "replacement-session", Status: string(pairing.SessionCancelled),
			},
		},
		errors: map[localapi.Method]error{
			localapi.MethodUnpair:     &localapi.PublicError{Code: localapi.ErrorRemoteRevokeUnavailable, Message: "remote trust revocation is unavailable"},
			localapi.MethodPairCancel: unavailable("pairing cancellation is unavailable"),
		},
	}
	controller, _ := NewDesktopController(supervisor, fallback)

	if _, err := controller.Handle(context.Background(), localapi.MethodReplaceDevice, json.RawMessage(
		`{"old_device_id":"saved","new_device":"new-windows"}`,
	)); err == nil {
		t.Fatal("ReplaceDevice double failure error = nil")
	}
	if !reflect.DeepEqual(fallback.methods, []localapi.Method{localapi.MethodPairStart, localapi.MethodUnpair, localapi.MethodPairCancel}) {
		t.Fatalf("double failure method order = %v", fallback.methods)
	}
	pending := supervisor.Snapshot()
	if pending.State != lifecycle.StatePairingCancellationPending || pending.Pairing == nil ||
		pending.Pairing.SessionID != "replacement-session" || pending.Pairing.Status != lifecycle.PairingCancellationPending ||
		pending.TrustedPeers != 1 || pending.Peer == nil || pending.Peer.ID != "saved" {
		t.Fatalf("hidden replacement cancellation = %#v", pending)
	}
	if _, err := controller.Handle(context.Background(), localapi.MethodReplaceDevice, json.RawMessage(
		`{"old_device_id":"saved","new_device":"new-windows"}`,
	)); err == nil {
		t.Fatal("second ReplaceDevice before cancellation error = nil")
	}
	if len(fallback.methods) != 3 {
		t.Fatalf("second replacement reached Bootstrap: %v", fallback.methods)
	}

	delete(fallback.errors, localapi.MethodPairCancel)
	if _, err := controller.Handle(context.Background(), localapi.MethodPairCancel, json.RawMessage(
		`{"session_id":"replacement-session"}`,
	)); err != nil {
		t.Fatalf("PairCancel retry error = %v", err)
	}
	cleared := supervisor.Snapshot()
	if cleared.State != lifecycle.StateSearching || cleared.Pairing != nil || cleared.TrustedPeers != 1 ||
		cleared.Peer == nil || cleared.Peer.ID != "saved" {
		t.Fatalf("retry cancellation changed old trust = %#v", cleared)
	}
}

func TestDesktopControllerObservesReplacementCancellationWithoutCommittingNewTrust(t *testing.T) {
	tests := []struct {
		name        string
		status      pairing.SessionState
		wantState   lifecycle.State
		wantPairing bool
	}{
		{name: "pending", status: pairing.SessionPending, wantState: lifecycle.StatePairingCancellationPending, wantPairing: true},
		{name: "approved", status: pairing.SessionApproved, wantState: lifecycle.StatePairingCancellationPending, wantPairing: true},
		{name: "completed", status: pairing.SessionCompleted, wantState: lifecycle.StatePairingCancellationPending, wantPairing: true},
		{name: "rejected", status: pairing.SessionRejected, wantState: lifecycle.StateSearching},
		{name: "expired", status: pairing.SessionExpired, wantState: lifecycle.StateSearching},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
			if err != nil {
				t.Fatalf("NewMachine() error = %v", err)
			}
			_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled})
			_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted})
			pending := lifecycle.Pairing{
				SessionID: "replacement-session", Peer: lifecycle.Peer{ID: "new", Name: "New Windows"},
				Code: "123456", ExpiresAt: time.Now().Add(time.Minute),
			}
			_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingCancellationPending, Pairing: &pending})
			supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
			fallback := &recordingLocalHandler{result: localapi.PairingStatusResult{
				SessionID: "replacement-session", Status: string(tt.status), Device: &localapi.Device{ID: "new"},
			}}
			controller, _ := NewDesktopController(supervisor, fallback)

			if _, err := controller.Handle(context.Background(), localapi.MethodPairStatus, json.RawMessage(
				`{"session_id":"replacement-session"}`,
			)); err != nil {
				t.Fatalf("PairStatus error = %v", err)
			}
			if !reflect.DeepEqual(fallback.methods, []localapi.Method{localapi.MethodPairStatus}) {
				t.Fatalf("delegated methods = %v", fallback.methods)
			}
			var params localapi.PairSessionParams
			if err := json.Unmarshal(fallback.raws[0], &params); err != nil || params.SessionID != "replacement-session" || !params.ObserveOnly {
				t.Fatalf("PairStatus params = %#v error=%v", params, err)
			}
			got := supervisor.Snapshot()
			if got.State != tt.wantState || (got.Pairing != nil) != tt.wantPairing || got.TrustedPeers != 1 || got.Peer == nil || got.Peer.ID != "saved" {
				t.Fatalf("snapshot = %#v", got)
			}
		})
	}
}

func TestDesktopControllerShutdownIgnoresReplacementCancelFailureAndStopsOwnedRuntime(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	runtime := newRecordingSessionRuntime()
	var stopContextErr error
	runtime.stop = func(ctx context.Context) error {
		stopContextErr = ctx.Err()
		return stopContextErr
	}
	watchdog := &recordingCrashWatchdog{}
	supervisor, _ := NewSupervisor(machine, runtime, WithWatchdogFactory(func() (CrashWatchdog, error) { return watchdog, nil }))
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted})
	pending := lifecycle.Pairing{
		SessionID: "replacement-session", Peer: lifecycle.Peer{ID: "new", Name: "New Windows"},
		Code: "123456", ExpiresAt: time.Now().Add(time.Minute),
	}
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingCancellationPending, Pairing: &pending})
	fallback := &shutdownPairingHandler{cancelErr: errors.New("remote cancellation unavailable")}
	controller, _ := NewDesktopController(supervisor, fallback)
	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := controller.Handle(requestCtx, localapi.MethodShutdown, nil)
	if err != nil {
		t.Fatalf("Shutdown error = %v", err)
	}
	if result != (localapi.ShutdownResult{Stopped: true}) || fallback.cancelled != "replacement-session" || fallback.cancelContextErr != nil || fallback.abandoned != "replacement-session" {
		t.Fatalf("shutdown result=%#v fallback=%#v", result, fallback)
	}
	if runtime.stopCalls != 1 || runtime.reason != lifecycle.StopQuit || stopContextErr != nil || watchdog.cleanStops != 1 {
		t.Fatalf("runtime stops=%d reason=%q context=%v watchdog=%d", runtime.stopCalls, runtime.reason, stopContextErr, watchdog.cleanStops)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StatePaused || !got.Terminal || got.Pairing != nil {
		t.Fatalf("terminal snapshot = %#v", got)
	}
	supervisor.mu.Lock()
	running, ownedWatchdog := supervisor.running, supervisor.watchdog
	supervisor.mu.Unlock()
	if running || ownedWatchdog != nil {
		t.Fatalf("owned runtime remains: running=%t watchdog=%#v", running, ownedWatchdog)
	}
}

func TestDesktopControllerReplaceRunsExactBootstrapForgetPairCommitOrder(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled})
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted})
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := &recordingLocalHandler{results: map[localapi.Method]any{
		localapi.MethodPairStart: localapi.PairStartResult{
			SessionID: "replacement-session", Code: "123456",
			ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			Peer:      localapi.LifecyclePeer{ID: "new-windows", Name: "New Windows", OS: "windows"},
		},
	}}
	controller, _ := NewDesktopController(supervisor, fallback)

	if _, err := controller.Handle(context.Background(), localapi.MethodReplaceDevice, json.RawMessage(
		`{"old_device_id":"saved","new_device":"new-windows","local_only":true}`,
	)); err != nil {
		t.Fatalf("ReplaceDevice error = %v", err)
	}
	if !reflect.DeepEqual(fallback.methods, []localapi.Method{localapi.MethodPairStart, localapi.MethodUnpair}) {
		t.Fatalf("replacement delegate order = %v", fallback.methods)
	}
	var unpair localapi.UnpairParams
	var start localapi.PairStartParams
	_ = json.Unmarshal(fallback.raws[0], &start)
	_ = json.Unmarshal(fallback.raws[1], &unpair)
	if unpair.DeviceID != "saved" || !unpair.LocalOnly || start.Device != "new-windows" {
		t.Fatalf("replacement params unpair=%#v start=%#v", unpair, start)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StatePairing || got.TrustedPeers != 0 || got.Peer != nil ||
		got.Pairing == nil || got.Pairing.SessionID != "replacement-session" {
		t.Fatalf("replacement snapshot = %#v", got)
	}
}

func TestDesktopControllerReplaceBlocksConcurrentLifecycleMutation(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled})
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted})
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := newBlockingReplaceHandler()
	controller, _ := NewDesktopController(supervisor, fallback)

	replaceDone := make(chan error, 1)
	go func() {
		_, err := controller.Handle(context.Background(), localapi.MethodReplaceDevice, json.RawMessage(
			`{"old_device_id":"saved","new_device":"new-windows","local_only":true}`,
		))
		replaceDone <- err
	}()
	waitForTestSignal(t, fallback.started, "replacement cleanup")
	for _, method := range []localapi.Method{localapi.MethodSearchStop, localapi.MethodPause} {
		if _, err := controller.Handle(context.Background(), method, nil); err == nil {
			t.Fatalf("concurrent %s error = nil", method)
		}
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateSearching || !got.ActionInProgress || got.TrustedPeers != 1 {
		t.Fatalf("reserved replacement snapshot = %#v", got)
	}
	close(fallback.release)
	if err := waitForTestError(t, replaceDone, "replacement completion"); err != nil {
		t.Fatalf("ReplaceDevice error = %v", err)
	}
	if got := fallback.Methods(); !reflect.DeepEqual(got, []localapi.Method{localapi.MethodPairStart, localapi.MethodUnpair}) {
		t.Fatalf("replacement methods = %v", got)
	}
}

func TestDesktopControllerReconnectsSavedDeviceWithoutStartingPairing(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := &recordingLocalHandler{}
	controller, _ := NewDesktopController(supervisor, fallback)

	if _, err := controller.Handle(context.Background(), localapi.MethodConnect, nil); err != nil {
		t.Fatalf("Connect error = %v", err)
	}
	if got := fallback.methods; len(got) != 1 || got[0] != localapi.MethodConnect {
		t.Fatalf("delegated methods = %v, want [Connect]", got)
	}
	snapshot := supervisor.Snapshot()
	if snapshot.TrustedPeers != 1 || snapshot.Peer == nil || snapshot.Peer.ID != "saved" {
		t.Fatalf("trusted device changed after reconnect = %#v", snapshot)
	}
}

func TestDesktopControllerRequiresWindowsApprovalBeforeCompletingPairing(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleWindowsHost)
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := &recordingLocalHandler{results: map[localapi.Method]any{
		localapi.MethodPairStatus: localapi.PairingStatusResult{
			SessionID: "session-1", Code: "654321", Status: "pending",
			ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			Peer:      localapi.LifecyclePeer{ID: "mac", Name: "Mac", OS: "macos"},
		},
		localapi.MethodPairApprove: localapi.PairingStatusResult{
			SessionID: "session-1", Code: "654321", Status: "approved",
			ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			Peer:      localapi.LifecyclePeer{ID: "mac", Name: "Mac", OS: "macos"},
		},
	}}
	controller, _ := NewDesktopController(supervisor, fallback)
	_, _ = controller.Handle(context.Background(), localapi.MethodEnable, nil)

	params := json.RawMessage(`{"session_id":"session-1"}`)
	if _, err := controller.Handle(context.Background(), localapi.MethodPairStatus, params); err != nil {
		t.Fatalf("PairStatus error = %v", err)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StatePairing || got.Pairing == nil || got.Pairing.Status != lifecycle.PairingPending {
		t.Fatalf("pending snapshot = %#v", got)
	}
	if _, err := controller.Handle(context.Background(), localapi.MethodPairApprove, params); err != nil {
		t.Fatalf("PairApprove error = %v", err)
	}
	if got := supervisor.Snapshot(); got.Pairing == nil || got.Pairing.Status != lifecycle.PairingApproved {
		t.Fatalf("approved snapshot = %#v", got)
	}
}

func TestDesktopControllerKeepsProblemPairingPollableAndCancelable(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	expires := time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	fallback := &recordingLocalHandler{results: map[localapi.Method]any{
		localapi.MethodPairStart: localapi.PairStartResult{
			SessionID: "session-1", Code: "123456", ExpiresAt: expires,
			Peer: localapi.LifecyclePeer{ID: "windows", Name: "Windows", OS: "windows"},
		},
		localapi.MethodPairCancel: localapi.PairingStatusResult{
			SessionID: "session-1", Code: "123456", Status: string(pairing.SessionCancelled), ExpiresAt: expires,
			Peer: localapi.LifecyclePeer{ID: "windows", Name: "Windows", OS: "windows"},
		},
	}}
	controller, _ := NewDesktopController(supervisor, fallback)
	_, _ = controller.Handle(context.Background(), localapi.MethodEnable, nil)
	_, _ = controller.Handle(context.Background(), localapi.MethodSearchStart, nil)
	_, _ = controller.Handle(context.Background(), localapi.MethodPairStart, json.RawMessage(`{"device":"windows"}`))
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventProblemDetected, Problem: &lifecycle.Problem{
		Code: "runtime_stopped", Message: "Runtime stopped", Action: "Retry",
	}})

	if _, err := controller.Handle(context.Background(), localapi.MethodPairCancel, json.RawMessage(`{"session_id":"session-1"}`)); err != nil {
		t.Fatalf("PairCancel after problem error = %v", err)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateNeedsAction || got.Pairing != nil || got.Problem == nil {
		t.Fatalf("snapshot after problem pairing cancel = %#v", got)
	}
}

func TestDesktopControllerCompletesMacPairingAfterRemoteApproval(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	expires := time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	fallback := &recordingLocalHandler{results: map[localapi.Method]any{
		localapi.MethodPairStart: localapi.PairStartResult{
			SessionID: "session-1", Code: "123456", ExpiresAt: expires,
			Peer: localapi.LifecyclePeer{ID: "temporary-windows", Name: "Windows PC", OS: "windows"},
		},
		localapi.MethodPairStatus: localapi.PairingStatusResult{
			SessionID: "session-1", Code: "123456", Status: "completed", ExpiresAt: expires,
			Peer:   localapi.LifecyclePeer{ID: "temporary-windows", Name: "Windows PC", OS: "windows"},
			Device: &localapi.Device{ID: "trusted-windows", Name: "Windows PC", Address: "192.168.1.20"},
		},
	}}
	controller, _ := NewDesktopController(supervisor, fallback)
	_, _ = controller.Handle(context.Background(), localapi.MethodEnable, nil)
	_, _ = controller.Handle(context.Background(), localapi.MethodSearchStart, nil)
	_, _ = controller.Handle(context.Background(), localapi.MethodPairStart, json.RawMessage(`{"device":"temporary-windows"}`))

	if _, err := controller.Handle(context.Background(), localapi.MethodPairStatus, json.RawMessage(`{"session_id":"session-1"}`)); err != nil {
		t.Fatalf("PairStatus error = %v", err)
	}
	got := supervisor.Snapshot()
	if got.State != lifecycle.StateConnecting || got.TrustedPeers != 1 || got.Peer == nil || got.Peer.ID != "trusted-windows" {
		t.Fatalf("completed snapshot = %#v", got)
	}
}

func TestDesktopControllerForgetsTrustOnlyAfterLocalCleanupSucceeds(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := &recordingLocalHandler{}
	controller, _ := NewDesktopController(supervisor, fallback)

	if _, err := controller.Handle(context.Background(), localapi.MethodForgetDevice, json.RawMessage(`{"device_id":"saved","local_only":true}`)); err != nil {
		t.Fatalf("ForgetDevice error = %v", err)
	}
	if got := supervisor.Snapshot(); got.TrustedPeers != 0 || got.Peer != nil {
		t.Fatalf("trusted snapshot after forget = %#v", got)
	}
	if len(fallback.methods) != 1 || fallback.methods[0] != localapi.MethodUnpair {
		t.Fatalf("delegated methods = %v, want [Unpair]", fallback.methods)
	}
	var params localapi.UnpairParams
	if err := json.Unmarshal(fallback.raws[0], &params); err != nil {
		t.Fatalf("decode delegated Unpair params: %v", err)
	}
	if params.DeviceID != "saved" || !params.LocalOnly {
		t.Fatalf("delegated Unpair params = %#v", params)
	}
}

func TestDesktopControllerKeepsTrustWhenLocalCleanupFails(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	cleanupErr := errors.New("managed SSH cleanup failed")
	fallback := &recordingLocalHandler{err: cleanupErr}
	controller, _ := NewDesktopController(supervisor, fallback)

	_, err = controller.Handle(context.Background(), localapi.MethodForgetDevice, json.RawMessage(`{"device_id":"saved","local_only":true}`))
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("ForgetDevice error = %v, want cleanup error", err)
	}
	if got := supervisor.Snapshot(); got.ActionInProgress || got.TrustedPeers != 1 || got.Peer == nil || got.Peer.ID != "saved" {
		t.Fatalf("trusted snapshot changed after cleanup failure = %#v", got)
	}
	fallback.err = nil
	if _, err := controller.Handle(context.Background(), localapi.MethodForgetDevice, json.RawMessage(`{"device_id":"saved","local_only":true}`)); err != nil {
		t.Fatalf("ForgetDevice retry error = %v", err)
	}
	if got := supervisor.Snapshot(); got.ActionInProgress || got.TrustedPeers != 0 || got.Peer != nil {
		t.Fatalf("trusted snapshot after retry = %#v", got)
	}
}

func TestDesktopControllerRejectsForgetWhileConnectingBeforeCleanup(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled})
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventConnectionStarted})
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := &recordingLocalHandler{}
	controller, _ := NewDesktopController(supervisor, fallback)

	_, err = controller.Handle(context.Background(), localapi.MethodForgetDevice, json.RawMessage(`{"device_id":"saved","local_only":true}`))
	var public *localapi.PublicError
	if !errors.As(err, &public) || public.Code != localapi.ErrorNeedsAction {
		t.Fatalf("ForgetDevice error = %v, want needs_action", err)
	}
	if len(fallback.methods) != 0 {
		t.Fatalf("cleanup methods = %v, want none", fallback.methods)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateConnecting || got.TrustedPeers != 1 || got.Peer == nil {
		t.Fatalf("connecting snapshot changed = %#v", got)
	}
}

func TestDesktopControllerForgetWinsPausedPrecheckRaceBeforeConnect(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	runtime := newRecordingSessionRuntime()
	supervisor, _ := NewSupervisor(machine, runtime)
	fallback := &blockingLocalHandler{started: make(chan struct{}), release: make(chan struct{})}
	controller, _ := NewDesktopController(supervisor, fallback)
	firstDone := make(chan error, 1)
	go func() {
		_, err := controller.Handle(context.Background(), localapi.MethodForgetDevice, json.RawMessage(`{"device_id":"saved","local_only":true}`))
		firstDone <- err
	}()
	waitForTestSignal(t, fallback.started, "forget cleanup start")

	if got := supervisor.Snapshot(); !got.ActionInProgress || got.TrustedPeers != 1 || got.Peer == nil {
		t.Fatalf("reserved snapshot = %#v", got)
	}
	conflicts := make([]chan error, 0, 2)
	for _, request := range []struct {
		method localapi.Method
		raw    json.RawMessage
	}{
		{method: localapi.MethodForgetDevice, raw: json.RawMessage(`{"device_id":"saved","local_only":true}`)},
		{method: localapi.MethodConnect},
	} {
		done := make(chan error, 1)
		conflicts = append(conflicts, done)
		go func(method localapi.Method, raw json.RawMessage) {
			_, err := controller.Handle(context.Background(), method, raw)
			done <- err
		}(request.method, request.raw)
	}
	if fallback.calls != 1 {
		t.Fatalf("cleanup calls = %d, want one", fallback.calls)
	}
	close(fallback.release)
	if err := waitForTestError(t, firstDone, "first forget completion"); err != nil {
		t.Fatalf("first ForgetDevice error = %v", err)
	}
	for _, done := range conflicts {
		if err := waitForTestError(t, done, "conflicting operation completion"); err == nil {
			t.Fatal("conflicting operation after forget error = nil")
		}
	}
	if got := supervisor.Snapshot(); got.ActionInProgress || got.TrustedPeers != 0 || got.Peer != nil {
		t.Fatalf("committed snapshot = %#v", got)
	}
	if runtime.startCalls != 0 {
		t.Fatalf("connect started runtime after forget won: calls=%d", runtime.startCalls)
	}
}

func TestDesktopControllerConnectWinsPausedPrecheckRaceBeforeForget(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	runtime := newRecordingSessionRuntime()
	supervisor, _ := NewSupervisor(machine, runtime)
	fallback := &blockingLocalHandler{started: make(chan struct{}), release: make(chan struct{})}
	controller, _ := NewDesktopController(supervisor, fallback)
	connectDone := make(chan error, 1)
	go func() {
		_, err := controller.Handle(context.Background(), localapi.MethodConnect, nil)
		connectDone <- err
	}()
	waitForTestSignal(t, fallback.started, "connect fallback start")
	forgetDone := make(chan error, 1)
	go func() {
		_, err := controller.Handle(context.Background(), localapi.MethodForgetDevice, json.RawMessage(`{"device_id":"saved","local_only":true}`))
		forgetDone <- err
	}()
	if fallback.calls != 1 {
		t.Fatalf("fallback calls before connect release = %d, want one", fallback.calls)
	}
	close(fallback.release)
	if err := waitForTestError(t, connectDone, "connect completion"); err != nil {
		t.Fatalf("Connect error = %v", err)
	}
	if err := waitForTestError(t, forgetDone, "forget completion"); err == nil {
		t.Fatal("ForgetDevice after winning Connect error = nil")
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateConnecting || got.TrustedPeers != 1 || got.Peer == nil {
		t.Fatalf("snapshot after connect/forget race = %#v", got)
	}
	if fallback.calls != 1 {
		t.Fatalf("forget reached cleanup fallback: calls=%d", fallback.calls)
	}
	if runtime.startCalls != 1 {
		t.Fatalf("connect runtime starts = %d, want one", runtime.startCalls)
	}
}

func TestDesktopControllerEnableWithTrustedPeerStartsConnectionBeforeForget(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	runtime := newRecordingSessionRuntime()
	runtime.onStart = func() {
		if got := machine.Snapshot(); got.State != lifecycle.StateConnecting || !got.ActionInProgress {
			t.Fatalf("snapshot at trusted runtime start = %#v", got)
		}
	}
	supervisor, _ := NewSupervisor(machine, runtime)
	fallback := &recordingLocalHandler{}
	controller, _ := NewDesktopController(supervisor, fallback)

	if _, err := controller.Handle(context.Background(), localapi.MethodEnable, nil); err != nil {
		t.Fatalf("Enable error = %v", err)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateConnecting || got.ActionInProgress || got.TrustedPeers != 1 {
		t.Fatalf("snapshot after trusted Enable = %#v", got)
	}
	if _, err := controller.Handle(context.Background(), localapi.MethodForgetDevice, json.RawMessage(`{"device_id":"saved","local_only":true}`)); err == nil {
		t.Fatal("ForgetDevice after trusted Enable error = nil")
	}
	if len(fallback.methods) != 0 {
		t.Fatalf("forget cleanup after trusted Enable = %v", fallback.methods)
	}
}

func TestDesktopControllerTrustedEnableStartFailureStopsAndJoinsBeforeReturningPaused(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	runtime := newRecordingSessionRuntime()
	runtime.startErr = errors.New("injected runtime start failure")
	runtime.onStart = func() {
		if got := machine.Snapshot(); got.State != lifecycle.StateConnecting || !got.ActionInProgress {
			t.Fatalf("snapshot at failed trusted runtime start = %#v", got)
		}
	}
	runtime.onStop = func() {
		if got := machine.Snapshot(); got.State != lifecycle.StateStopping || !got.ActionInProgress {
			t.Fatalf("snapshot during trusted runtime cleanup = %#v", got)
		}
	}
	supervisor, _ := NewSupervisor(machine, runtime)
	controller, _ := NewDesktopController(supervisor, &recordingLocalHandler{})

	if _, err := controller.Handle(context.Background(), localapi.MethodEnable, nil); err == nil {
		t.Fatal("trusted Enable start failure error = nil")
	}
	if runtime.startCalls != 1 || runtime.stopCalls != 1 || runtime.reason != lifecycle.StopFailure {
		t.Fatalf("runtime calls after failed trusted Enable: starts=%d stops=%d reason=%q", runtime.startCalls, runtime.stopCalls, runtime.reason)
	}
	if got := machine.Snapshot(); got.State != lifecycle.StatePaused || got.ActionInProgress || !machine.Allowed(lifecycle.CommandForget) {
		t.Fatalf("snapshot after failed trusted Enable = %#v", got)
	}
}

func TestDesktopControllerRejectsInternalUnpairEntryPoint(t *testing.T) {
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := &recordingLocalHandler{}
	controller, _ := NewDesktopController(supervisor, fallback)

	_, err = controller.Handle(context.Background(), localapi.MethodUnpair, json.RawMessage(`{"device_id":"saved","local_only":true}`))
	var public *localapi.PublicError
	if !errors.As(err, &public) || public.Code != localapi.ErrorInvalidRequest {
		t.Fatalf("internal Unpair error = %v, want invalid_request", err)
	}
	if len(fallback.methods) != 0 || supervisor.Snapshot().TrustedPeers != 1 {
		t.Fatalf("internal Unpair mutated state: methods=%v snapshot=%#v", fallback.methods, supervisor.Snapshot())
	}
}

type blockingLocalHandler struct {
	started chan struct{}
	release chan struct{}
	calls   int
}

type blockingPairStartHandler struct {
	mu           sync.Mutex
	started      chan struct{}
	release      chan struct{}
	startOnce    sync.Once
	starts       int
	cancelled    string
	expiresAt    string
	afterStart   func()
	cancelCtxErr error
	cancelErr    error
}

type blockingReplaceHandler struct {
	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
	methods []localapi.Method
}

func newBlockingReplaceHandler() *blockingReplaceHandler {
	return &blockingReplaceHandler{started: make(chan struct{}), release: make(chan struct{})}
}

func (h *blockingReplaceHandler) Handle(_ context.Context, method localapi.Method, _ json.RawMessage) (any, error) {
	h.mu.Lock()
	h.methods = append(h.methods, method)
	h.mu.Unlock()
	switch method {
	case localapi.MethodUnpair:
		close(h.started)
		<-h.release
		return map[string]bool{"unpaired": true}, nil
	case localapi.MethodPairStart:
		return localapi.PairStartResult{
			SessionID: "replacement-session", Code: "123456",
			ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			Peer:      localapi.LifecyclePeer{ID: "new-windows", Name: "New Windows", OS: "windows"},
		}, nil
	default:
		return nil, nil
	}
}

func (h *blockingReplaceHandler) Methods() []localapi.Method {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]localapi.Method(nil), h.methods...)
}

func newBlockingPairStartHandler() *blockingPairStartHandler {
	return &blockingPairStartHandler{
		started:   make(chan struct{}),
		release:   make(chan struct{}),
		expiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
	}
}

func (h *blockingPairStartHandler) Handle(ctx context.Context, method localapi.Method, raw json.RawMessage) (any, error) {
	switch method {
	case localapi.MethodPairStart:
		h.mu.Lock()
		h.starts++
		h.mu.Unlock()
		h.startOnce.Do(func() { close(h.started) })
		<-h.release
		if h.afterStart != nil {
			h.afterStart()
		}
		return localapi.PairStartResult{
			SessionID: "session-1", Code: "123456", ExpiresAt: h.expiresAt,
			Peer: localapi.LifecyclePeer{ID: "windows", Name: "Windows PC", OS: "windows"},
		}, nil
	case localapi.MethodPairCancel:
		var params localapi.PairSessionParams
		_ = json.Unmarshal(raw, &params)
		h.mu.Lock()
		h.cancelled = params.SessionID
		h.cancelCtxErr = ctx.Err()
		h.mu.Unlock()
		return localapi.PairingStatusResult{SessionID: params.SessionID, Status: string(pairing.SessionCancelled)}, h.cancelErr
	default:
		return nil, nil
	}
}

func (h *blockingPairStartHandler) StartCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.starts
}

func (h *blockingPairStartHandler) CancelledSession() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cancelled
}

func (h *blockingPairStartHandler) CancelContextError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cancelCtxErr
}

func closedSignal() chan struct{} {
	signal := make(chan struct{})
	close(signal)
	return signal
}

func (h *blockingLocalHandler) Handle(context.Context, localapi.Method, json.RawMessage) (any, error) {
	h.calls++
	close(h.started)
	<-h.release
	return nil, nil
}

func waitForTestSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for %s", name)
	}
}

func waitForTestError(t *testing.T, result <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for %s", name)
		return nil
	}
}

type recordingLocalHandler struct {
	methods []localapi.Method
	raws    []json.RawMessage
	result  any
	results map[localapi.Method]any
	errors  map[localapi.Method]error
	err     error
}

func (h *recordingLocalHandler) Handle(_ context.Context, method localapi.Method, raw json.RawMessage) (any, error) {
	h.methods = append(h.methods, method)
	h.raws = append(h.raws, append(json.RawMessage(nil), raw...))
	if err, ok := h.errors[method]; ok {
		return h.results[method], err
	}
	if result, ok := h.results[method]; ok {
		return result, h.err
	}
	return h.result, h.err
}

type shutdownPairingHandler struct {
	cancelled        string
	cancelContextErr error
	cancelErr        error
	abandoned        string
}

func (h *shutdownPairingHandler) Handle(ctx context.Context, method localapi.Method, raw json.RawMessage) (any, error) {
	if method != localapi.MethodPairCancel {
		return nil, errors.New("unexpected method")
	}
	var params localapi.PairSessionParams
	_ = json.Unmarshal(raw, &params)
	h.cancelled = params.SessionID
	h.cancelContextErr = ctx.Err()
	return nil, h.cancelErr
}

func (h *shutdownPairingHandler) abandonPairing(sessionID string) {
	h.abandoned = sessionID
}
