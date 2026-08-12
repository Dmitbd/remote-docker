package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
)

func TestSupervisorDoesNotStartRuntimeUntilEnabled(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	runtime := newRecordingSessionRuntime()
	supervisor, err := NewSupervisor(machine, runtime)
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	if runtime.startCalls != 0 || runtime.stopCalls != 0 || supervisor.Snapshot().State != lifecycle.StatePaused {
		t.Fatalf("construction started work: runtime=%#v snapshot=%#v", runtime, supervisor.Snapshot())
	}
}

func TestSupervisorStartIsIdempotent(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	runtime := newRecordingSessionRuntime()
	supervisor, err := NewSupervisor(machine, runtime)
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	if runtime.startCalls != 1 || runtime.role != lifecycle.RoleMacClient {
		t.Fatalf("runtime start calls=%d role=%q", runtime.startCalls, runtime.role)
	}
	if supervisor.Snapshot().State != lifecycle.StateClientReady {
		t.Fatalf("started state = %q", supervisor.Snapshot().State)
	}
}

func TestSupervisorPausePublishesStoppingBeforeRuntimeCleanup(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleWindowsHost)
	runtime := newRecordingSessionRuntime()
	runtime.onStop = func() {
		if got := machine.Snapshot().State; got != lifecycle.StateStopping {
			t.Fatalf("state during runtime Stop() = %q, want %q", got, lifecycle.StateStopping)
		}
	}
	supervisor, _ := NewSupervisor(machine, runtime)
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := supervisor.Pause(context.Background()); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	if runtime.stopCalls != 1 || runtime.reason != lifecycle.StopPause {
		t.Fatalf("runtime stop calls=%d reason=%q", runtime.stopCalls, runtime.reason)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StatePaused || got.ActionInProgress {
		t.Fatalf("paused snapshot = %#v", got)
	}
	if err := supervisor.Pause(context.Background()); err != nil || runtime.stopCalls != 1 {
		t.Fatalf("idempotent Pause() error=%v stopCalls=%d", err, runtime.stopCalls)
	}
}

func TestSupervisorArmsWatchdogOnlyForEnabledSessionAndStopsItCleanly(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	runtime := newRecordingSessionRuntime()
	watchdog := &recordingCrashWatchdog{}
	factoryCalls := 0
	supervisor, err := NewSupervisor(machine, runtime, WithWatchdogFactory(func() (CrashWatchdog, error) {
		factoryCalls++
		return watchdog, nil
	}))
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	if factoryCalls != 0 {
		t.Fatalf("watchdog factory ran while paused: %d", factoryCalls)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if factoryCalls != 1 {
		t.Fatalf("watchdog factory calls = %d, want 1", factoryCalls)
	}
	if err := supervisor.Pause(context.Background()); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	if watchdog.cleanStops != 1 {
		t.Fatalf("watchdog clean stops = %d, want 1", watchdog.cleanStops)
	}
}

func TestSupervisorUsesBoundedStopContext(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleWindowsHost)
	runtime := newRecordingSessionRuntime()
	runtime.stop = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	supervisor, err := NewSupervisor(machine, runtime, WithSupervisorStopTimeout(5*time.Millisecond))
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := supervisor.Pause(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Pause() error = %v, want deadline exceeded", err)
	}
	if supervisor.Snapshot().State != lifecycle.StateNeedsAction {
		t.Fatalf("failed cleanup state = %q, want needs action", supervisor.Snapshot().State)
	}
}

func TestSupervisorPropagatesUnexpectedRuntimeFailure(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	runtime := newRecordingSessionRuntime()
	supervisor, _ := NewSupervisor(machine, runtime)
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	runtime.fail(errors.New("private child output must not be shown"))

	deadline := time.After(time.Second)
	for supervisor.Snapshot().State != lifecycle.StateNeedsAction {
		select {
		case <-deadline:
			t.Fatalf("runtime failure was not published: %#v", supervisor.Snapshot())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	problem := supervisor.Snapshot().Problem
	if problem == nil || problem.Code != "runtime_stopped" || problem.Message == "private child output must not be shown" {
		t.Fatalf("runtime problem = %#v", problem)
	}
}

func TestSupervisorPublishesTypedLocalSyncIdentityProblem(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	runtime := newRecordingSessionRuntime()
	runtime.startErr = &localSyncIdentityBlockedError{
		cause: errors.New("keychain=/private/account token=secret"),
	}
	supervisor, err := NewSupervisor(machine, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(context.Background()); err == nil {
		t.Fatal("Start() succeeded for blocked identity recovery")
	}
	problem := supervisor.Snapshot().Problem
	if problem == nil || problem.Code != "local_sync_identity_corrupt" {
		t.Fatalf("problem = %#v, want local_sync_identity_corrupt", problem)
	}
	publicText := problem.Message + " " + problem.Action
	for _, forbidden := range []string{"private", "secret", "keychain", "token"} {
		if strings.Contains(strings.ToLower(publicText), forbidden) {
			t.Fatalf("problem leaked %q: %#v", forbidden, problem)
		}
	}
}

func TestSupervisorShutdownIsTerminalAndIdempotent(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	runtime := newRecordingSessionRuntime()
	supervisor, _ := NewSupervisor(machine, runtime)
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
	if runtime.stopCalls != 1 || runtime.reason != lifecycle.StopQuit {
		t.Fatalf("runtime stop calls=%d reason=%q", runtime.stopCalls, runtime.reason)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StatePaused || !got.Terminal {
		t.Fatalf("terminal snapshot = %#v", got)
	}
}

func TestSupervisorCancelConnectionStopsOwnedRuntimeOnce(t *testing.T) {
	trusted := lifecycle.Peer{ID: "windows", Name: "Windows"}
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(trusted))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	runtime := newRecordingSessionRuntime()
	stopStarted := make(chan struct{})
	releaseStop := make(chan struct{})
	runtime.onStop = func() {
		if got := machine.Snapshot().State; got != lifecycle.StateStopping {
			t.Errorf("state during runtime Stop() = %q, want %q", got, lifecycle.StateStopping)
		}
		close(stopStarted)
		<-releaseStop
	}
	supervisor, err := NewSupervisor(machine, runtime)
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventConnectionStarted}); err != nil {
		t.Fatalf("Apply(EventConnectionStarted) error = %v", err)
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- supervisor.CancelConnection(context.Background()) }()
	select {
	case <-stopStarted:
	case <-time.After(time.Second):
		t.Fatal("runtime Stop() did not start")
	}

	secondDone := make(chan error, 1)
	go func() { secondDone <- supervisor.CancelConnection(context.Background()) }()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second CancelConnection() error = %v", err)
		}
	case <-time.After(time.Second):
		close(releaseStop)
		t.Fatal("second CancelConnection() blocked while stop was already active")
	}
	if calls, reason := runtime.stopSnapshot(); calls != 1 || reason != lifecycle.StopCancelConnection {
		close(releaseStop)
		t.Fatalf("runtime stop calls=%d reason=%q", calls, reason)
	}

	close(releaseStop)
	if err := <-firstDone; err != nil {
		t.Fatalf("first CancelConnection() error = %v", err)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateClientReady || got.ActionInProgress ||
		got.TrustedPeers != 1 || got.Peer == nil || got.Peer.ID != trusted.ID {
		t.Fatalf("stopped connection snapshot = %#v", got)
	}
}

func TestSupervisorCancelConnectionCancelsAndJoinsInflightStart(t *testing.T) {
	trusted := lifecycle.Peer{ID: "windows", Name: "Windows"}
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(trusted))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventConnectionStartReserved}); err != nil {
		t.Fatalf("Apply(EventConnectionStartReserved) error = %v", err)
	}
	runtime := newRecordingSessionRuntime()
	startEntered := make(chan struct{})
	startResolved := make(chan struct{})
	stopEntered := make(chan struct{})
	releaseStop := make(chan struct{})
	var releaseOnce sync.Once
	releaseCleanup := func() { releaseOnce.Do(func() { close(releaseStop) }) }
	runtime.start = func(ctx context.Context) error {
		close(startEntered)
		<-ctx.Done()
		close(startResolved)
		return ctx.Err()
	}
	runtime.stop = func(context.Context) error {
		close(stopEntered)
		<-releaseStop
		return nil
	}
	supervisor, err := NewSupervisor(machine, runtime)
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	startCtx, cancelStart := context.WithCancel(context.Background())
	defer cancelStart()
	defer releaseCleanup()
	startDone := make(chan error, 1)
	go func() { startDone <- supervisor.Start(startCtx) }()
	select {
	case <-startEntered:
	case <-time.After(time.Second):
		t.Fatal("runtime Start() did not begin")
	}

	cancelDone := make(chan error, 1)
	go func() { cancelDone <- supervisor.CancelConnection(context.Background()) }()
	select {
	case <-startResolved:
	case err := <-cancelDone:
		t.Fatalf("CancelConnection() returned before startup resolved: %v", err)
	case <-time.After(time.Second):
		t.Fatal("CancelConnection() did not cancel in-flight startup")
	}
	select {
	case <-stopEntered:
	case <-time.After(time.Second):
		t.Fatal("CancelConnection() did not start owned cleanup after startup resolved")
	}
	select {
	case err := <-cancelDone:
		t.Fatalf("CancelConnection() completed before owned cleanup: %v", err)
	default:
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateStopping || !got.ActionInProgress {
		t.Fatalf("snapshot before cleanup completion = %#v", got)
	}

	releaseCleanup()
	if err := <-cancelDone; err != nil {
		t.Fatalf("CancelConnection() error = %v", err)
	}
	if err := <-startDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context cancellation", err)
	}
	if calls, reason := runtime.stopSnapshot(); calls != 1 || reason != lifecycle.StopCancelConnection {
		t.Fatalf("runtime stop calls=%d reason=%q", calls, reason)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateClientReady || got.ActionInProgress ||
		got.TrustedPeers != 1 || got.Peer == nil || got.Peer.ID != trusted.ID {
		t.Fatalf("post-cancel snapshot = %#v", got)
	}
}

func TestSupervisorCancelConnectionRetriesFailedForcedRuntimeStop(t *testing.T) {
	trusted := lifecycle.Peer{ID: "windows", Name: "Windows"}
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(trusted))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventConnectionStartReserved}); err != nil {
		t.Fatalf("Apply(EventConnectionStartReserved) error = %v", err)
	}
	runtime := newRecordingSessionRuntime()
	startEntered := make(chan struct{})
	stopFailure := errors.New("forced runtime stop failed")
	runtime.start = func(ctx context.Context) error {
		close(startEntered)
		<-ctx.Done()
		return ctx.Err()
	}
	runtime.stop = func(context.Context) error {
		calls, _ := runtime.stopSnapshot()
		if calls == 1 {
			return stopFailure
		}
		return nil
	}
	supervisor, err := NewSupervisor(machine, runtime)
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	startDone := make(chan error, 1)
	go func() { startDone <- supervisor.Start(context.Background()) }()
	select {
	case <-startEntered:
	case <-time.After(time.Second):
		t.Fatal("runtime Start() did not begin")
	}

	if err := supervisor.CancelConnection(context.Background()); !errors.Is(err, stopFailure) {
		t.Fatalf("first CancelConnection() error = %v, want stop failure", err)
	}
	if err := <-startDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context cancellation", err)
	}
	retryable := supervisor.Snapshot()
	if retryable.State != lifecycle.StateConnecting || retryable.ActionInProgress || retryable.TrustedPeers != 1 ||
		retryable.Peer == nil || retryable.Peer.ID != trusted.ID || !machine.Allowed(lifecycle.CommandCancelConnection) {
		t.Fatalf("retryable forced-stop snapshot = %#v", retryable)
	}

	if err := supervisor.CancelConnection(context.Background()); err != nil {
		t.Fatalf("second CancelConnection() error = %v", err)
	}
	if calls, reason := runtime.stopSnapshot(); calls != 2 || reason != lifecycle.StopCancelConnection {
		t.Fatalf("runtime stop calls=%d reason=%q", calls, reason)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateClientReady || got.ActionInProgress ||
		got.TrustedPeers != 1 || got.Peer == nil || got.Peer.ID != trusted.ID {
		t.Fatalf("post-retry snapshot = %#v", got)
	}
}

func TestSupervisorCancelConnectionDoesNotStartRuntimeAfterStartupLeaseIsCancelled(t *testing.T) {
	trusted := lifecycle.Peer{ID: "windows", Name: "Windows"}
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(trusted))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventConnectionStartReserved}); err != nil {
		t.Fatalf("Apply(EventConnectionStartReserved) error = %v", err)
	}
	runtime := newRecordingSessionRuntime()
	watchdog := &recordingCrashWatchdog{}
	factoryEntered := make(chan struct{})
	releaseFactory := make(chan struct{})
	supervisor, err := NewSupervisor(machine, runtime, WithWatchdogFactory(func() (CrashWatchdog, error) {
		close(factoryEntered)
		<-releaseFactory
		return watchdog, nil
	}))
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	startDone := make(chan error, 1)
	go func() { startDone <- supervisor.Start(context.Background()) }()
	select {
	case <-factoryEntered:
	case <-time.After(time.Second):
		t.Fatal("watchdog factory did not begin")
	}

	cancelDone := make(chan error, 1)
	updates, unsubscribe := machine.Subscribe()
	defer unsubscribe()
	<-updates
	go func() { cancelDone <- supervisor.CancelConnection(context.Background()) }()
	for snapshot := range updates {
		if snapshot.State == lifecycle.StateStopping {
			break
		}
	}
	close(releaseFactory)
	if err := <-startDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context cancellation", err)
	}
	if err := <-cancelDone; err != nil {
		t.Fatalf("CancelConnection() error = %v", err)
	}
	if runtime.startCalls != 0 || runtime.stopCalls != 0 || watchdog.cleanStops != 1 {
		t.Fatalf("cancelled pre-runtime startup: runtime=%#v watchdog clean stops=%d", runtime, watchdog.cleanStops)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateClientReady || got.ActionInProgress ||
		got.TrustedPeers != 1 || got.Peer == nil || got.Peer.ID != trusted.ID {
		t.Fatalf("post-cancel snapshot = %#v", got)
	}
}

func TestSupervisorCancelConnectionRetriesFailedPairingStop(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	runtime := newRecordingSessionRuntime()
	stopFailure := errors.New("owned runtime is still stopping")
	runtime.stop = func(context.Context) error {
		calls, _ := runtime.stopSnapshot()
		if calls == 1 {
			return stopFailure
		}
		return nil
	}
	supervisor, err := NewSupervisor(machine, runtime)
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted}); err != nil {
		t.Fatalf("Apply(EventSearchStarted) error = %v", err)
	}
	pairing := lifecycle.Pairing{
		SessionID: "session-retry", Peer: lifecycle.Peer{ID: "windows", Name: "Windows"}, Code: "123456",
	}
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingStarted, Pairing: &pairing}); err != nil {
		t.Fatalf("Apply(EventPairingStarted) error = %v", err)
	}

	if err := supervisor.CancelConnection(context.Background()); !errors.Is(err, stopFailure) {
		t.Fatalf("first CancelConnection() error = %v, want stop failure", err)
	}
	retryable := supervisor.Snapshot()
	if retryable.State != lifecycle.StatePairing || retryable.ActionInProgress || retryable.Problem != nil ||
		retryable.Pairing == nil || retryable.Pairing.SessionID != pairing.SessionID || retryable.Pairing.Code != pairing.Code ||
		!machine.Allowed(lifecycle.CommandCancelConnection) {
		t.Fatalf("retryable pairing snapshot = %#v", retryable)
	}

	if err := supervisor.CancelConnection(context.Background()); err != nil {
		t.Fatalf("second CancelConnection() error = %v", err)
	}
	if calls, reason := runtime.stopSnapshot(); calls != 2 || reason != lifecycle.StopCancelConnection {
		t.Fatalf("runtime stop calls=%d reason=%q", calls, reason)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateClientReady || got.Pairing != nil || got.ActionInProgress {
		t.Fatalf("retried cancellation snapshot = %#v", got)
	}
}

func TestSupervisorCancelConnectionRetriesWatchdogJoinBeforePublishingIdle(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	runtime := newRecordingSessionRuntime()
	joinFailure := errors.New("watchdog join timed out")
	watchdog := &retryingCrashWatchdog{errors: []error{joinFailure, nil}}
	supervisor, err := NewSupervisor(machine, runtime, WithWatchdogFactory(func() (CrashWatchdog, error) {
		return watchdog, nil
	}))
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted}); err != nil {
		t.Fatalf("Apply(EventSearchStarted) error = %v", err)
	}
	pairing := lifecycle.Pairing{
		SessionID: "session-watchdog-retry", Peer: lifecycle.Peer{ID: "windows", Name: "Windows"}, Code: "123456",
	}
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingStarted, Pairing: &pairing}); err != nil {
		t.Fatalf("Apply(EventPairingStarted) error = %v", err)
	}

	if err := supervisor.CancelConnection(context.Background()); !errors.Is(err, joinFailure) {
		t.Fatalf("first CancelConnection() error = %v, want watchdog join failure", err)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StatePairing || got.Pairing == nil ||
		got.Pairing.SessionID != pairing.SessionID || got.Pairing.Code != pairing.Code || got.ActionInProgress {
		t.Fatalf("retryable watchdog snapshot = %#v", got)
	}
	if err := supervisor.CancelConnection(context.Background()); err != nil {
		t.Fatalf("second CancelConnection() error = %v", err)
	}
	if calls, _ := runtime.stopSnapshot(); calls != 1 {
		t.Fatalf("runtime stop calls = %d, want successful runtime cleanup to remain one-shot", calls)
	}
	if watchdog.calls != 2 {
		t.Fatalf("watchdog clean/join calls = %d, want 2", watchdog.calls)
	}
	if got := supervisor.Snapshot(); got.State != lifecycle.StateClientReady || got.Pairing != nil || got.ActionInProgress {
		t.Fatalf("post-watchdog-retry snapshot = %#v", got)
	}
}

func newLifecycleMachine(t *testing.T, role lifecycle.Role) *lifecycle.Machine {
	t.Helper()
	machine, err := lifecycle.NewMachine(role, "Device")
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	return machine
}

type recordingSessionRuntime struct {
	mu         sync.Mutex
	startCalls int
	stopCalls  int
	role       lifecycle.Role
	reason     lifecycle.StopReason
	done       chan error
	onStart    func()
	onStop     func()
	startErr   error
	start      func(context.Context) error
	stop       func(context.Context) error
}

type recordingCrashWatchdog struct{ cleanStops int }

type retryingCrashWatchdog struct {
	calls  int
	errors []error
}

func (w *recordingCrashWatchdog) CleanStop(context.Context) error {
	w.cleanStops++
	return nil
}

func (w *retryingCrashWatchdog) CleanStop(context.Context) error {
	w.calls++
	if w.calls <= len(w.errors) {
		return w.errors[w.calls-1]
	}
	return nil
}

func newRecordingSessionRuntime() *recordingSessionRuntime {
	return &recordingSessionRuntime{done: make(chan error, 1)}
}

func (r *recordingSessionRuntime) Start(ctx context.Context, role lifecycle.Role) error {
	r.mu.Lock()
	r.startCalls++
	r.role = role
	onStart := r.onStart
	startErr := r.startErr
	start := r.start
	r.mu.Unlock()
	if onStart != nil {
		onStart()
	}
	if start != nil {
		return start(ctx)
	}
	return startErr
}

func (r *recordingSessionRuntime) Stop(ctx context.Context, reason lifecycle.StopReason) error {
	r.mu.Lock()
	r.stopCalls++
	r.reason = reason
	onStop := r.onStop
	stop := r.stop
	r.mu.Unlock()
	if onStop != nil {
		onStop()
	}
	if stop != nil {
		return stop(ctx)
	}
	return nil
}

func (r *recordingSessionRuntime) Done() <-chan error { return r.done }

func (r *recordingSessionRuntime) fail(err error) { r.done <- err }

func (r *recordingSessionRuntime) stopSnapshot() (int, lifecycle.StopReason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopCalls, r.reason
}
