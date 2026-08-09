package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
	"github.com/Dmitbd/remote-docker/internal/localapi"
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
	supervisor, _ := NewSupervisor(machine, newRecordingSessionRuntime())
	fallback := &recordingLocalHandler{}
	controller, _ := NewDesktopController(supervisor, fallback)

	if _, err := controller.Handle(context.Background(), localapi.MethodEnable, nil); err != nil {
		t.Fatalf("Enable error = %v", err)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateConnecting || got.TrustedPeers != 1 {
		t.Fatalf("snapshot after trusted Enable = %#v", got)
	}
	if _, err := controller.Handle(context.Background(), localapi.MethodForgetDevice, json.RawMessage(`{"device_id":"saved","local_only":true}`)); err == nil {
		t.Fatal("ForgetDevice after trusted Enable error = nil")
	}
	if len(fallback.methods) != 0 {
		t.Fatalf("forget cleanup after trusted Enable = %v", fallback.methods)
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
	err     error
}

func (h *recordingLocalHandler) Handle(_ context.Context, method localapi.Method, raw json.RawMessage) (any, error) {
	h.methods = append(h.methods, method)
	h.raws = append(h.raws, append(json.RawMessage(nil), raw...))
	if result, ok := h.results[method]; ok {
		return result, h.err
	}
	return h.result, h.err
}
