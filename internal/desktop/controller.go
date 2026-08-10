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

var (
	ErrConnectionLimit  = errors.New("trusted-device connection limit is occupied")
	ErrPairingNotActive = errors.New("new-device pairing is available only while searching")
)

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
	if action == ActionConnect {
		snapshot := c.snapshot()
		if snapshot.State != lifecycle.StateSearching {
			return ErrPairingNotActive
		}
		if connectionLimitOccupied(snapshot) {
			return ErrConnectionLimit
		}
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

func (c *Controller) Candidates(ctx context.Context) ([]localapi.PairingCandidate, error) {
	if c == nil || c.handler == nil {
		return nil, errors.New("desktop controller is unavailable")
	}
	result, err := c.handler.Handle(ctx, localapi.MethodPairCandidates, nil)
	if err != nil {
		return nil, err
	}
	candidates, ok := result.(localapi.PairCandidatesResult)
	if !ok {
		return nil, errors.New("device search returned an invalid response")
	}
	return append([]localapi.PairingCandidate(nil), candidates.Candidates...), nil
}

func (c *Controller) ForgetDevice(ctx context.Context, deviceID string, localOnly bool) error {
	if c == nil || c.handler == nil {
		return errors.New("desktop controller is unavailable")
	}
	raw, err := json.Marshal(localapi.ForgetDeviceParams{
		DeviceID:  strings.TrimSpace(deviceID),
		LocalOnly: localOnly,
	})
	if err != nil {
		return errors.New("desktop action could not be encoded")
	}
	_, err = c.handler.Handle(ctx, localapi.MethodForgetDevice, raw)
	return err
}

func (c *Controller) ReplaceDevice(ctx context.Context, oldDeviceID, newDevice string, localOnly bool) error {
	if c == nil || c.handler == nil {
		return errors.New("desktop controller is unavailable")
	}
	raw, err := json.Marshal(localapi.ReplaceDeviceParams{
		OldDeviceID: strings.TrimSpace(oldDeviceID),
		NewDevice:   strings.TrimSpace(newDevice),
		LocalOnly:   localOnly,
	})
	if err != nil {
		return errors.New("desktop action could not be encoded")
	}
	_, err = c.handler.Handle(ctx, localapi.MethodReplaceDevice, raw)
	return err
}

func (c *Controller) PollPairing(ctx context.Context) (localapi.PairingStatusResult, error) {
	if c == nil || c.handler == nil || c.snapshot == nil {
		return localapi.PairingStatusResult{}, errors.New("desktop controller is unavailable")
	}
	params := localapi.PairSessionParams{}
	if pairing := c.snapshot().Pairing; pairing != nil {
		params.SessionID = pairing.SessionID
	}
	raw, _ := json.Marshal(params)
	result, err := c.handler.Handle(ctx, localapi.MethodPairStatus, raw)
	if err != nil {
		return localapi.PairingStatusResult{}, err
	}
	status, ok := result.(localapi.PairingStatusResult)
	if !ok {
		return localapi.PairingStatusResult{}, errors.New("pairing status returned an invalid response")
	}
	return status, nil
}

func (c *Controller) Workspaces(ctx context.Context) ([]localapi.Workspace, error) {
	result, err := c.handler.Handle(ctx, localapi.MethodWorkspaceList, nil)
	if err != nil {
		return nil, err
	}
	workspaces, ok := result.(localapi.WorkspaceListResult)
	if !ok {
		return nil, errors.New("workspace list returned an invalid response")
	}
	return append([]localapi.Workspace(nil), workspaces.Workspaces...), nil
}

func (c *Controller) RemoveWorkspace(ctx context.Context, id string) error {
	raw, _ := json.Marshal(localapi.WorkspaceRemoveParams{ID: strings.TrimSpace(id)})
	_, err := c.handler.Handle(ctx, localapi.MethodWorkspaceRemove, raw)
	return err
}

func (c *Controller) Diagnostics(ctx context.Context) ([]localapi.DoctorCheck, error) {
	result, err := c.handler.Handle(ctx, localapi.MethodDoctor, nil)
	if err != nil {
		return nil, err
	}
	diagnostics, ok := result.(localapi.DoctorResult)
	if !ok {
		return nil, errors.New("diagnostics returned an invalid response")
	}
	return append([]localapi.DoctorCheck(nil), diagnostics.Checks...), nil
}

func (c *Controller) Resources(ctx context.Context) (localapi.ResourceStatusResult, error) {
	if c == nil || c.handler == nil {
		return localapi.ResourceStatusResult{}, errors.New("desktop controller is unavailable")
	}
	result, err := c.handler.Handle(ctx, localapi.MethodResourceStatus, nil)
	if err != nil {
		return localapi.ResourceStatusResult{}, err
	}
	resources, ok := result.(localapi.ResourceStatusResult)
	if !ok {
		return localapi.ResourceStatusResult{}, errors.New("resource status returned an invalid response")
	}
	return resources, nil
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
	case ActionConnectTrusted:
		return localapi.MethodConnect, nil, strings.TrimSpace(value) != ""
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
		return localapi.MethodForgetDevice, localapi.ForgetDeviceParams{DeviceID: strings.TrimSpace(value)}, true
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

func connectionLimitOccupied(snapshot lifecycle.Snapshot) bool {
	limit := snapshot.ConnectionLimit
	if limit <= 0 {
		limit = 1
	}
	return snapshot.TrustedPeers >= limit
}
