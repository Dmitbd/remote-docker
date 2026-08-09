package app

import (
	"context"
	"errors"
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
	onStop     func()
	stop       func(context.Context) error
}

type recordingCrashWatchdog struct{ cleanStops int }

func (w *recordingCrashWatchdog) CleanStop(context.Context) error {
	w.cleanStops++
	return nil
}

func newRecordingSessionRuntime() *recordingSessionRuntime {
	return &recordingSessionRuntime{done: make(chan error, 1)}
}

func (r *recordingSessionRuntime) Start(_ context.Context, role lifecycle.Role) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startCalls++
	r.role = role
	return nil
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
