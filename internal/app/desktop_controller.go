package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
	"github.com/Dmitbd/remote-docker/internal/localapi"
)

// DesktopController is the single mutation boundary shared by the window,
// tray, and command-line client. Presentation code never controls processes.
type DesktopController struct {
	supervisor *Supervisor
	fallback   localapi.Handler
}

func NewDesktopController(supervisor *Supervisor, fallback localapi.Handler) (*DesktopController, error) {
	if supervisor == nil {
		return nil, errors.New("desktop lifecycle supervisor is required")
	}
	return &DesktopController{supervisor: supervisor, fallback: fallback}, nil
}

func (c *DesktopController) Handle(ctx context.Context, method localapi.Method, raw json.RawMessage) (any, error) {
	if c == nil || c.supervisor == nil {
		return nil, unavailable("desktop lifecycle is unavailable")
	}
	switch method {
	case localapi.MethodStatus:
		return statusFromLifecycle(c.supervisor.Snapshot()), nil
	case localapi.MethodEnable:
		if err := c.supervisor.Start(ctx); err != nil {
			return nil, unavailable("Remote Docker could not be enabled")
		}
		return c.actionResult(), nil
	case localapi.MethodPause:
		if err := c.supervisor.Pause(ctx); err != nil {
			return nil, unavailable("Remote Docker could not be paused safely")
		}
		return c.actionResult(), nil
	case localapi.MethodSearchStart:
		if _, err := c.supervisor.machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted}); err != nil {
			return nil, needsAction("enable the Mac client before starting search")
		}
		return c.actionResult(), nil
	case localapi.MethodSearchStop:
		if _, err := c.supervisor.machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStopped}); err != nil {
			return nil, needsAction("device search is not active")
		}
		return c.actionResult(), nil
	case localapi.MethodDisconnect:
		params := localapi.DisconnectParams{}
		if err := decodeOptionalControlParams(raw, &params); err != nil {
			return nil, err
		}
		disconnect := lifecycle.Disconnect{Initiator: lifecycle.InitiatorLocal, Reason: lifecycle.ReasonUserDisconnect}
		if err := c.supervisor.Disconnect(ctx, disconnect); err != nil {
			return nil, needsAction("no active Remote Docker connection exists")
		}
		return c.actionResult(), nil
	case localapi.MethodForgetDevice:
		var params localapi.ForgetDeviceParams
		if err := decodeOptionalControlParams(raw, &params); err != nil {
			return nil, err
		}
		snapshot := c.supervisor.Snapshot()
		if snapshot.Peer == nil || strings.TrimSpace(params.DeviceID) != "" && params.DeviceID != snapshot.Peer.ID {
			return nil, needsAction("trusted device was not found")
		}
		if c.fallback != nil {
			unpairRaw, _ := json.Marshal(localapi.UnpairParams{DeviceID: snapshot.Peer.ID})
			if _, err := c.fallback.Handle(ctx, localapi.MethodUnpair, unpairRaw); err != nil {
				return nil, err
			}
		}
		if _, err := c.supervisor.machine.Apply(lifecycle.Event{Type: lifecycle.EventTrustForgotten}); err != nil {
			return nil, needsAction("disconnect before forgetting the trusted device")
		}
		return c.actionResult(), nil
	case localapi.MethodShutdown:
		if err := c.supervisor.Shutdown(ctx); err != nil {
			return nil, unavailable("Remote Docker could not stop every owned process")
		}
		return localapi.ShutdownResult{Stopped: true}, nil
	case localapi.MethodPrepareDocker:
		snapshot := c.supervisor.Snapshot()
		if snapshot.Role != lifecycle.RoleMacClient || snapshot.State != lifecycle.StateConnected {
			return nil, needsAction("open Remote Docker and connect to the Windows Docker host")
		}
		return c.delegate(ctx, method, raw)
	case localapi.MethodPairStatus, localapi.MethodPairApprove, localapi.MethodPairReject, localapi.MethodPairCancel:
		return nil, needsAction("the pairing request is not ready for this action")
	case localapi.MethodResourceStatus:
		return nil, unavailable("resource monitoring is not available yet")
	default:
		return c.delegate(ctx, method, raw)
	}
}

func (c *DesktopController) actionResult() localapi.LifecycleActionResult {
	return localapi.LifecycleActionResult{Status: statusFromLifecycle(c.supervisor.Snapshot())}
}

func (c *DesktopController) delegate(ctx context.Context, method localapi.Method, raw json.RawMessage) (any, error) {
	if c.fallback == nil {
		return nil, unavailable("desktop operation is unavailable")
	}
	return c.fallback.Handle(ctx, method, raw)
}

func decodeOptionalControlParams(raw json.RawMessage, destination any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return decodeControlParams(raw, destination)
}

func statusFromLifecycle(snapshot lifecycle.Snapshot) localapi.StatusResult {
	result := localapi.StatusResult{
		Revision: snapshot.Revision, Role: string(snapshot.Role), State: string(snapshot.State),
		LocalName: snapshot.LocalName, Paired: snapshot.TrustedPeers == 1,
		TrustedPeers: snapshot.TrustedPeers, ConnectionLimit: snapshot.ConnectionLimit,
		Docker: localapi.ServiceStatus{State: string(snapshot.Docker.State), Message: snapshot.Docker.Message},
		Sync: localapi.ServiceStatus{State: string(snapshot.Sync.State), Pending: snapshot.Sync.Pending},
		LatencyMS: snapshot.Latency.Milliseconds(), ActionInProgress: snapshot.ActionInProgress,
		Terminal: snapshot.Terminal,
	}
	if snapshot.Peer != nil {
		result.Peer = lifecyclePeer(*snapshot.Peer)
	}
	if snapshot.Pairing != nil {
		result.Pairing = &localapi.PairingStatusResult{
			SessionID: snapshot.Pairing.SessionID, Peer: *lifecyclePeer(snapshot.Pairing.Peer),
			Code: snapshot.Pairing.Code, Status: string(snapshot.Pairing.Status),
			ExpiresAt: formatLifecycleTime(snapshot.Pairing.ExpiresAt),
		}
	}
	if !snapshot.Sync.LastSuccess.IsZero() {
		result.Sync.LastSuccess = formatLifecycleTime(snapshot.Sync.LastSuccess)
	}
	if snapshot.Recovery != nil {
		result.Recovery = &localapi.RecoveryStatus{Deadline: formatLifecycleTime(snapshot.Recovery.Deadline)}
	}
	if snapshot.LastDisconnect != nil {
		result.LastDisconnect = &localapi.DisconnectStatus{
			Initiator: string(snapshot.LastDisconnect.Initiator), Reason: string(snapshot.LastDisconnect.Reason),
			At: formatLifecycleTime(snapshot.LastDisconnect.At),
		}
	}
	if snapshot.Problem != nil {
		result.Problem = &localapi.ProblemStatus{
			Code: snapshot.Problem.Code, Device: string(snapshot.Problem.Device),
			Message: snapshot.Problem.Message, Action: snapshot.Problem.Action,
		}
		result.Message = snapshot.Problem.Message
	}
	return result
}

func lifecyclePeer(peer lifecycle.Peer) *localapi.LifecyclePeer {
	return &localapi.LifecyclePeer{ID: peer.ID, Name: peer.Name, OS: peer.OS, Version: peer.Version, Address: peer.Address}
}

func formatLifecycleTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

var _ localapi.Handler = (*DesktopController)(nil)
