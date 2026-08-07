package tray

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/localapi"
)

func TestMenuForStatusMapsEveryAgentStateToStablePresentation(t *testing.T) {
	t.Parallel()

	wantActions := []Action{
		ActionPair,
		ActionOpenStatus,
		ActionAddWorkspace,
		ActionRetry,
		ActionRunDiagnostics,
		ActionUnpair,
		ActionQuit,
	}
	tests := []struct {
		state string
		label string
		icon  Icon
	}{
		{state: "Unpaired", label: "Not paired", icon: IconUnpaired},
		{state: "Connecting", label: "Connecting", icon: IconConnecting},
		{state: "EngineStarting", label: "Starting Docker Engine", icon: IconStarting},
		{state: "Syncing", label: "Syncing workspaces", icon: IconSyncing},
		{state: "Ready", label: "Ready", icon: IconReady},
		{state: "Degraded", label: "Connection needs attention", icon: IconDegraded},
		{state: "NeedsAction", label: "Action required", icon: IconNeedsAction},
		{state: "unknown", label: "Action required", icon: IconNeedsAction},
	}

	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			model := MenuForStatus(localapi.StatusResult{State: tt.state, Message: "agent message"})
			if model.Label != tt.label || model.Icon != tt.icon {
				t.Fatalf("MenuForStatus(%q) = label=%q icon=%q, want label=%q icon=%q", tt.state, model.Label, model.Icon, tt.label, tt.icon)
			}
			if model.Message != "agent message" {
				t.Fatalf("message = %q, want agent message", model.Message)
			}
			if got := actionIDs(model.Items); !reflect.DeepEqual(got, wantActions) {
				t.Fatalf("menu actions = %#v, want %#v", got, wantActions)
			}
			if containsDestructiveAction(model.Items) {
				t.Fatalf("menu contains a destructive action: %#v", model.Items)
			}
		})
	}
}

func TestControllerPairShowsSelectedDeviceAndRequiresExplicitConfirmation(t *testing.T) {
	t.Parallel()

	client := &recordingClient{results: map[localapi.Method]any{
		localapi.MethodPairStart:   localapi.PairStartResult{SessionID: "session-1", Code: "123456"},
		localapi.MethodPairConfirm: localapi.PairConfirmResult{Device: localapi.Device{ID: "device-1", Name: "Windows host"}},
		localapi.MethodStatus:      localapi.StatusResult{State: "Ready", Message: "connected"},
	}}
	controller := NewController(client)

	model, err := controller.Pair(context.Background(), "Windows host")
	if err != nil {
		t.Fatalf("Pair error = %v", err)
	}
	if model.Pairing == nil || model.Pairing.DeviceName != "Windows host" || model.Pairing.Code != "123456" {
		t.Fatalf("pairing = %#v, want selected device and six-digit code", model.Pairing)
	}
	if !hasAction(model.Items, ActionConfirmPair) {
		t.Fatalf("menu items = %#v, want explicit confirm action", model.Items)
	}
	if client.callsFor(localapi.MethodPairConfirm) != 0 {
		t.Fatal("Pair called PairConfirm without an explicit confirmation action")
	}

	if _, err := controller.ConfirmPair(context.Background()); err != nil {
		t.Fatalf("ConfirmPair error = %v", err)
	}
	if client.callsFor(localapi.MethodPairConfirm) != 1 {
		t.Fatalf("PairConfirm calls = %d, want 1", client.callsFor(localapi.MethodPairConfirm))
	}
	params, ok := client.lastParams(localapi.MethodPairConfirm).(localapi.PairConfirmParams)
	if !ok || params.SessionID != "session-1" || params.Code != "123456" {
		t.Fatalf("PairConfirm params = %#v, want session and code", params)
	}
}

func TestControllerUsesOnlyLocalAPIForAgentActionsAndBoundsCalls(t *testing.T) {
	t.Parallel()

	client := &recordingClient{results: map[localapi.Method]any{
		localapi.MethodStatus:       localapi.StatusResult{State: "Ready"},
		localapi.MethodRecover:      localapi.RecoverResult{State: "Ready"},
		localapi.MethodDoctor:       localapi.DoctorResult{},
		localapi.MethodUnpair:       map[string]bool{"unpaired": true},
		localapi.MethodWorkspaceAdd: localapi.Workspace{ID: "workspace-1", Path: "/work"},
	}}
	controller := NewController(client)
	controller.Timeout = 20 * time.Millisecond

	if _, err := controller.OpenStatus(context.Background()); err != nil {
		t.Fatalf("OpenStatus error = %v", err)
	}
	if _, err := controller.AddWorkspace(context.Background(), "/work"); err != nil {
		t.Fatalf("AddWorkspace error = %v", err)
	}
	if _, err := controller.Retry(context.Background()); err != nil {
		t.Fatalf("Retry error = %v", err)
	}
	if _, err := controller.RunDiagnostics(context.Background()); err != nil {
		t.Fatalf("RunDiagnostics error = %v", err)
	}
	if _, err := controller.Unpair(context.Background(), "device-1"); err != nil {
		t.Fatalf("Unpair error = %v", err)
	}

	wantMethods := []localapi.Method{
		localapi.MethodStatus,
		localapi.MethodWorkspaceAdd,
		localapi.MethodRecover,
		localapi.MethodDoctor,
		localapi.MethodUnpair,
	}
	if got := client.methods(); !reflect.DeepEqual(got, wantMethods) {
		t.Fatalf("local API methods = %#v, want %#v", got, wantMethods)
	}
	workspace, ok := client.lastParams(localapi.MethodWorkspaceAdd).(localapi.WorkspaceAddParams)
	if !ok || workspace.Path != "/work" {
		t.Fatalf("WorkspaceAdd params = %#v", workspace)
	}
}

func TestControllerSurfacesSafeErrorText(t *testing.T) {
	t.Parallel()

	controller := NewController(&recordingClient{errors: map[localapi.Method]error{
		localapi.MethodStatus: errors.New("dial unix /private/secret: connection refused"),
	}})
	model, err := controller.OpenStatus(context.Background())
	if err == nil {
		t.Fatal("OpenStatus error = nil, want safe error")
	}
	if model.Message != unavailableMessage || err.Error() != unavailableMessage {
		t.Fatalf("safe error = model %q / err %q, want %q", model.Message, err, unavailableMessage)
	}
}

type recordingClient struct {
	results map[localapi.Method]any
	errors  map[localapi.Method]error
	calls   []recordedCall
}

type recordedCall struct {
	method localapi.Method
	params any
}

func (c *recordingClient) Call(_ context.Context, method localapi.Method, params any, destination any) error {
	c.calls = append(c.calls, recordedCall{method: method, params: params})
	if err := c.errors[method]; err != nil {
		return err
	}
	result, ok := c.results[method]
	if !ok || destination == nil {
		return nil
	}
	reflect.ValueOf(destination).Elem().Set(reflect.ValueOf(result))
	return nil
}

func (c *recordingClient) methods() []localapi.Method {
	methods := make([]localapi.Method, 0, len(c.calls))
	for _, call := range c.calls {
		methods = append(methods, call.method)
	}
	return methods
}

func (c *recordingClient) callsFor(method localapi.Method) int {
	count := 0
	for _, call := range c.calls {
		if call.method == method {
			count++
		}
	}
	return count
}

func (c *recordingClient) lastParams(method localapi.Method) any {
	for index := len(c.calls) - 1; index >= 0; index-- {
		if c.calls[index].method == method {
			return c.calls[index].params
		}
	}
	return nil
}
