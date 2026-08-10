package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
	"github.com/Dmitbd/remote-docker/internal/localapi"
)

func TestControllerMapsStableActionsToOwnerOnlyMethods(t *testing.T) {
	handler := &recordingHandler{}
	controller := NewController(handler, func() lifecycle.Snapshot {
		return lifecycle.Snapshot{Pairing: &lifecycle.Pairing{SessionID: "session"}}
	})
	for _, tt := range []struct {
		action ActionID
		method localapi.Method
	}{
		{ActionEnableClient, localapi.MethodEnable}, {ActionEnableHost, localapi.MethodEnable},
		{ActionStartSearch, localapi.MethodSearchStart}, {ActionStopSearch, localapi.MethodSearchStop},
		{ActionApprovePair, localapi.MethodPairApprove}, {ActionRejectPair, localapi.MethodPairReject},
		{ActionCancelPair, localapi.MethodPairCancel}, {ActionPause, localapi.MethodPause},
		{ActionDisconnect, localapi.MethodDisconnect}, {ActionForgetDevice, localapi.MethodForgetDevice},
		{ActionQuit, localapi.MethodShutdown},
	} {
		if err := controller.Perform(context.Background(), tt.action, ""); err != nil {
			t.Fatalf("Perform(%s) error = %v", tt.action, err)
		}
		if handler.method != tt.method {
			t.Fatalf("Perform(%s) method = %s, want %s", tt.action, handler.method, tt.method)
		}
	}
}

func TestControllerRejectsNewPairBeforeDelegatingWhenLimitIsFull(t *testing.T) {
	handler := &recordingHandler{}
	controller := NewController(handler, func() lifecycle.Snapshot {
		return lifecycle.Snapshot{State: lifecycle.StateSearching, TrustedPeers: 1, ConnectionLimit: 1}
	})

	err := controller.Perform(context.Background(), ActionConnect, "new-windows")
	if !errors.Is(err, ErrConnectionLimit) {
		t.Fatalf("Perform(Connect) error = %v, want ErrConnectionLimit", err)
	}
	if handler.method != "" {
		t.Fatalf("Perform(Connect) delegated method = %q, want none", handler.method)
	}
}

func TestControllerReconnectsTrustedDeviceWithoutPairStart(t *testing.T) {
	handler := &recordingHandler{}
	controller := NewController(handler, func() lifecycle.Snapshot {
		return lifecycle.Snapshot{TrustedPeers: 1, ConnectionLimit: 1}
	})

	if err := controller.Perform(context.Background(), ActionConnectTrusted, "saved-windows"); err != nil {
		t.Fatalf("Perform(ConnectTrusted) error = %v", err)
	}
	if handler.method != localapi.MethodConnect {
		t.Fatalf("Perform(ConnectTrusted) method = %q, want %q", handler.method, localapi.MethodConnect)
	}
}

func TestControllerRejectsEverySecondConnectionDuringOccupiedStates(t *testing.T) {
	for _, state := range []lifecycle.State{
		lifecycle.StatePairing, lifecycle.StateConnecting, lifecycle.StateConnected, lifecycle.StateReconnecting,
	} {
		for _, action := range []ActionID{ActionConnect, ActionConnectTrusted} {
			t.Run(string(state)+"/"+string(action), func(t *testing.T) {
				handler := &recordingHandler{}
				controller := NewController(handler, func() lifecycle.Snapshot {
					return lifecycle.Snapshot{State: state, TrustedPeers: 1, ConnectionLimit: 1}
				})
				if err := controller.Perform(context.Background(), action, "another-device"); !errors.Is(err, ErrConnectionLimit) {
					t.Fatalf("Perform(%s) error = %v, want ErrConnectionLimit", action, err)
				}
				if handler.method != "" {
					t.Fatalf("Perform(%s) delegated %q while %s", action, handler.method, state)
				}
			})
		}
	}
}

func TestControllerForgetsSelectedDeviceWithExplicitScope(t *testing.T) {
	handler := &recordingHandler{}
	controller := NewController(handler, func() lifecycle.Snapshot { return lifecycle.Snapshot{} })

	if err := controller.ForgetDevice(context.Background(), "saved-windows", true); err != nil {
		t.Fatalf("ForgetDevice() error = %v", err)
	}
	var params localapi.ForgetDeviceParams
	if err := json.Unmarshal(handler.params, &params); err != nil {
		t.Fatalf("ForgetDevice params error = %v", err)
	}
	if handler.method != localapi.MethodForgetDevice || params.DeviceID != "saved-windows" || !params.LocalOnly {
		t.Fatalf("ForgetDevice method=%s params=%#v", handler.method, params)
	}
}

func TestControllerReplacesExactDeviceWithOneAtomicMethod(t *testing.T) {
	handler := &recordingHandler{}
	controller := NewController(handler, func() lifecycle.Snapshot {
		return lifecycle.Snapshot{State: lifecycle.StateSearching, TrustedPeers: 1, ConnectionLimit: 1}
	})

	if err := controller.ReplaceDevice(context.Background(), "saved-windows", "new-windows", true); err != nil {
		t.Fatalf("ReplaceDevice() error = %v", err)
	}
	var params localapi.ReplaceDeviceParams
	if err := json.Unmarshal(handler.params, &params); err != nil {
		t.Fatalf("ReplaceDevice params error = %v", err)
	}
	if handler.method != localapi.MethodReplaceDevice || params.OldDeviceID != "saved-windows" ||
		params.NewDevice != "new-windows" || !params.LocalOnly {
		t.Fatalf("ReplaceDevice method=%s params=%#v", handler.method, params)
	}
}

func TestControllerAddsOnlySelectedWorkspacePath(t *testing.T) {
	handler := &recordingHandler{}
	controller := NewController(handler, func() lifecycle.Snapshot { return lifecycle.Snapshot{} })
	if err := controller.Perform(context.Background(), ActionAddWorkspace, "/Users/demo/project"); err != nil {
		t.Fatalf("Perform(AddWorkspace) error = %v", err)
	}
	var params localapi.WorkspaceAddParams
	if err := json.Unmarshal(handler.params, &params); err != nil || params.Path != "/Users/demo/project" {
		t.Fatalf("workspace params = %#v error=%v", params, err)
	}
}

func TestControllerReadsCandidatesAndPollsPairingWithoutManualCode(t *testing.T) {
	handler := &recordingHandler{results: map[localapi.Method]any{
		localapi.MethodPairCandidates: localapi.PairCandidatesResult{Candidates: []localapi.PairingCandidate{{ID: "pc", Name: "Windows PC"}}},
		localapi.MethodPairStatus:     localapi.PairingStatusResult{SessionID: "session", Code: "123456", Status: "pending"},
	}}
	controller := NewController(handler, func() lifecycle.Snapshot {
		return lifecycle.Snapshot{Pairing: &lifecycle.Pairing{SessionID: "session", Code: "123456"}}
	})
	candidates, err := controller.Candidates(context.Background())
	if err != nil || len(candidates) != 1 || candidates[0].ID != "pc" {
		t.Fatalf("Candidates() = %#v, %v", candidates, err)
	}
	status, err := controller.PollPairing(context.Background())
	if err != nil || status.SessionID != "session" || status.Status != "pending" {
		t.Fatalf("PollPairing() = %#v, %v", status, err)
	}
	if _, ok := handler.lastPairParams.(localapi.PairSessionParams); !ok {
		t.Fatalf("pair status params = %#v", handler.lastPairParams)
	}
}

func TestControllerReadsWorkspacesAndDiagnostics(t *testing.T) {
	handler := &recordingHandler{results: map[localapi.Method]any{
		localapi.MethodWorkspaceList: localapi.WorkspaceListResult{Workspaces: []localapi.Workspace{{ID: "one", Path: "/project"}}},
		localapi.MethodDoctor:        localapi.DoctorResult{Checks: []localapi.DoctorCheck{{Name: "docker", OK: true}}},
	}}
	controller := NewController(handler, func() lifecycle.Snapshot { return lifecycle.Snapshot{} })
	workspaces, err := controller.Workspaces(context.Background())
	if err != nil || len(workspaces) != 1 || workspaces[0].ID != "one" {
		t.Fatalf("Workspaces() = %#v, %v", workspaces, err)
	}
	checks, err := controller.Diagnostics(context.Background())
	if err != nil || len(checks) != 1 || !checks[0].OK {
		t.Fatalf("Diagnostics() = %#v, %v", checks, err)
	}
	if err := controller.RemoveWorkspace(context.Background(), "one"); err != nil || handler.method != localapi.MethodWorkspaceRemove {
		t.Fatalf("RemoveWorkspace() method=%s error=%v", handler.method, err)
	}
}

type recordingHandler struct {
	method         localapi.Method
	params         json.RawMessage
	results        map[localapi.Method]any
	lastPairParams any
}

func (h *recordingHandler) Handle(_ context.Context, method localapi.Method, params json.RawMessage) (any, error) {
	h.method = method
	h.params = append(json.RawMessage(nil), params...)
	if method == localapi.MethodPairStatus {
		var decoded localapi.PairSessionParams
		_ = json.Unmarshal(params, &decoded)
		h.lastPairParams = decoded
	}
	return h.results[method], nil
}
