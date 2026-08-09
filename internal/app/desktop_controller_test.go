package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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

type recordingLocalHandler struct {
	methods []localapi.Method
	result  any
	err     error
}

func (h *recordingLocalHandler) Handle(_ context.Context, method localapi.Method, _ json.RawMessage) (any, error) {
	h.methods = append(h.methods, method)
	return h.result, h.err
}
