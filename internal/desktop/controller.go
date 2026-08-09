package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
	"github.com/Dmitbd/remote-docker/internal/localapi"
)

type SnapshotProvider func() lifecycle.Snapshot

type Controller struct {
	handler  localapi.Handler
	snapshot SnapshotProvider
}

func NewController(handler localapi.Handler, snapshot SnapshotProvider) *Controller {
	return &Controller{handler: handler, snapshot: snapshot}
}

func (c *Controller) Perform(ctx context.Context, action ActionID, value string) error {
	if c == nil || c.handler == nil || c.snapshot == nil {
		return errors.New("desktop controller is unavailable")
	}
	method, params, ok := c.resolve(action, value)
	if !ok {
		return errors.New("desktop action is unavailable")
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return errors.New("desktop action could not be encoded")
	}
	_, err = c.handler.Handle(ctx, method, raw)
	return err
}

func (c *Controller) resolve(action ActionID, value string) (localapi.Method, any, bool) {
	switch action {
	case ActionEnableClient, ActionEnableHost:
		return localapi.MethodEnable, nil, true
	case ActionStartSearch:
		return localapi.MethodSearchStart, nil, true
	case ActionStopSearch:
		return localapi.MethodSearchStop, nil, true
	case ActionConnect:
		return localapi.MethodPairStart, localapi.PairStartParams{Device: strings.TrimSpace(value)}, strings.TrimSpace(value) != ""
	case ActionApprovePair, ActionRejectPair, ActionCancelPair:
		snapshot := c.snapshot()
		if snapshot.Pairing == nil || snapshot.Pairing.SessionID == "" {
			return "", nil, false
		}
		method := localapi.MethodPairApprove
		if action == ActionRejectPair {
			method = localapi.MethodPairReject
		} else if action == ActionCancelPair {
			method = localapi.MethodPairCancel
		}
		return method, localapi.PairSessionParams{SessionID: snapshot.Pairing.SessionID}, true
	case ActionPause:
		return localapi.MethodPause, nil, true
	case ActionDisconnect:
		return localapi.MethodDisconnect, localapi.DisconnectParams{}, true
	case ActionForgetDevice:
		return localapi.MethodForgetDevice, localapi.ForgetDeviceParams{}, true
	case ActionAddWorkspace:
		value = strings.TrimSpace(value)
		return localapi.MethodWorkspaceAdd, localapi.WorkspaceAddParams{Path: value}, value != ""
	case ActionDiagnostics:
		return localapi.MethodDoctor, nil, true
	case ActionQuit:
		return localapi.MethodShutdown, nil, true
	default:
		return "", nil, false
	}
}
