package desktop

import (
	"context"
	"encoding/json"
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

type recordingHandler struct {
	method localapi.Method
	params json.RawMessage
}

func (h *recordingHandler) Handle(_ context.Context, method localapi.Method, params json.RawMessage) (any, error) {
	h.method = method
	h.params = append(json.RawMessage(nil), params...)
	return nil, nil
}
