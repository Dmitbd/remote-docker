package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/pairing"
)

const pairingRollbackTimeout = 5 * time.Second

// DesktopController is the single mutation boundary shared by the window,
// tray, and command-line client. Presentation code never controls processes.
type DesktopController struct {
	supervisor *Supervisor
	fallback   localapi.Handler
	operations sync.Mutex
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
		if !c.operations.TryLock() {
			return nil, needsAction("wait for the active lifecycle operation to finish")
		}
		defer c.operations.Unlock()
		if snapshot := c.supervisor.Snapshot(); snapshot.TrustedPeers == 1 && snapshot.Peer != nil {
			if _, err := c.startTrustedConnection(ctx, nil); err != nil {
				return nil, err
			}
			return c.actionResult(), nil
		}
		if err := c.supervisor.Start(ctx); err != nil {
			return nil, unavailable("Remote Docker could not be enabled")
		}
		return c.actionResult(), nil
	case localapi.MethodPause:
		if !c.operations.TryLock() {
			return nil, needsAction("wait for the active lifecycle operation to finish")
		}
		defer c.operations.Unlock()
		if err := c.supervisor.Pause(ctx); err != nil {
			return nil, unavailable("Remote Docker could not be paused safely")
		}
		return c.actionResult(), nil
	case localapi.MethodSearchStart:
		if !c.operations.TryLock() {
			return nil, needsAction("wait for the active lifecycle operation to finish")
		}
		defer c.operations.Unlock()
		if _, err := c.supervisor.machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted}); err != nil {
			return nil, needsAction("enable the Mac client before starting search")
		}
		return c.actionResult(), nil
	case localapi.MethodSearchStop:
		if !c.operations.TryLock() {
			return nil, needsAction("wait for the active lifecycle operation to finish")
		}
		defer c.operations.Unlock()
		if _, err := c.supervisor.machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStopped}); err != nil {
			return nil, needsAction("device search is not active")
		}
		return c.actionResult(), nil
	case localapi.MethodPairStart:
		if !c.operations.TryLock() {
			return nil, needsAction("wait for the active lifecycle operation to finish")
		}
		defer c.operations.Unlock()
		if connectionLimitOccupied(c.supervisor.Snapshot()) {
			return nil, needsAction("the trusted-device limit is occupied")
		}
		if snapshot := c.supervisor.Snapshot(); snapshot.Role != lifecycle.RoleMacClient ||
			snapshot.State != lifecycle.StateSearching || snapshot.Pairing != nil {
			return nil, needsAction("the device is not ready to start pairing")
		}
		result, err := c.delegate(ctx, method, raw)
		if err != nil {
			return nil, err
		}
		started, ok := result.(localapi.PairStartResult)
		if !ok {
			return nil, unavailable("pairing returned an invalid response")
		}
		if err := c.startPairing(started); err != nil {
			return c.rollbackStartedPairing(started, err)
		}
		return started, nil
	case localapi.MethodReplaceDevice:
		if !c.operations.TryLock() {
			return nil, needsAction("wait for the active lifecycle operation to finish")
		}
		defer c.operations.Unlock()
		var params localapi.ReplaceDeviceParams
		if err := decodeOptionalControlParams(raw, &params); err != nil {
			return nil, err
		}
		return c.replaceDeviceLocked(ctx, params)
	case localapi.MethodConnect:
		if !c.operations.TryLock() {
			return nil, needsAction("wait for the active lifecycle operation to finish")
		}
		defer c.operations.Unlock()
		if snapshot := c.supervisor.Snapshot(); snapshot.TrustedPeers < 1 || snapshot.Peer == nil {
			return nil, needsAction("trusted device was not found")
		}
		return c.startTrustedConnection(ctx, func() (any, error) { return c.delegate(ctx, method, raw) })
	case localapi.MethodPairStatus, localapi.MethodPairApprove, localapi.MethodPairReject, localapi.MethodPairCancel:
		if !c.operations.TryLock() {
			return nil, needsAction("wait for the active lifecycle operation to finish")
		}
		defer c.operations.Unlock()
		snapshot := c.supervisor.Snapshot()
		if snapshot.State == lifecycle.StatePairingCancellationPending {
			if method != localapi.MethodPairStatus && method != localapi.MethodPairCancel {
				return nil, needsAction("only replacement cancellation can be retried")
			}
			if snapshot.Pairing == nil {
				return nil, unavailable("replacement cancellation metadata is unavailable")
			}
			raw, _ = json.Marshal(localapi.PairSessionParams{
				SessionID: snapshot.Pairing.SessionID, ObserveOnly: method == localapi.MethodPairStatus,
			})
		}
		result, err := c.delegate(ctx, method, raw)
		if err != nil {
			return nil, err
		}
		status, ok := result.(localapi.PairingStatusResult)
		if !ok {
			return nil, unavailable("pairing returned an invalid response")
		}
		if err := c.reconcilePairing(status); err != nil {
			return nil, err
		}
		return status, nil
	case localapi.MethodDisconnect:
		if !c.operations.TryLock() {
			return nil, needsAction("wait for the active lifecycle operation to finish")
		}
		defer c.operations.Unlock()
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
		if !c.operations.TryLock() {
			return nil, needsAction("wait for the active connection operation to finish")
		}
		defer c.operations.Unlock()
		var params localapi.ForgetDeviceParams
		if err := decodeOptionalControlParams(raw, &params); err != nil {
			return nil, err
		}
		return c.forgetDeviceLocked(ctx, params)
	case localapi.MethodUnpair:
		return nil, &localapi.PublicError{Code: localapi.ErrorInvalidRequest, Message: "Unpair is an internal cleanup operation"}
	case localapi.MethodShutdown:
		cancellationPending := c.supervisor.Snapshot().State == lifecycle.StatePairingCancellationPending
		if cancellationPending {
			c.operations.Lock()
		} else if !c.operations.TryLock() {
			return nil, needsAction("wait for the active lifecycle operation to finish")
		}
		defer c.operations.Unlock()
		if snapshot := c.supervisor.Snapshot(); snapshot.State == lifecycle.StatePairingCancellationPending && snapshot.Pairing != nil {
			sessionID := snapshot.Pairing.SessionID
			cancelCtx, cancel := context.WithTimeout(context.Background(), pairingRollbackTimeout)
			cancelRaw, _ := json.Marshal(localapi.PairSessionParams{SessionID: sessionID})
			_, _ = c.delegate(cancelCtx, localapi.MethodPairCancel, cancelRaw)
			cancel()
			if cleaner, ok := c.fallback.(interface{ abandonPairing(string) }); ok {
				cleaner.abandonPairing(sessionID)
			}
		}
		shutdownCtx := ctx
		if cancellationPending {
			shutdownCtx = context.Background()
		}
		if err := c.supervisor.Shutdown(shutdownCtx); err != nil {
			return nil, unavailable("Remote Docker could not stop every owned process")
		}
		return localapi.ShutdownResult{Stopped: true}, nil
	case localapi.MethodPrepareDocker:
		snapshot := c.supervisor.Snapshot()
		if snapshot.Role != lifecycle.RoleMacClient || snapshot.State != lifecycle.StateConnected {
			return nil, needsAction("open Remote Docker and connect to the Windows Docker host")
		}
		return c.delegate(ctx, method, raw)
	case localapi.MethodResourceStatus:
		snapshot := c.supervisor.Snapshot()
		active := snapshot.State == lifecycle.StateConnected || snapshot.State == lifecycle.StateConnecting || snapshot.State == lifecycle.StateReconnecting
		params, _ := json.Marshal(localapi.ResourceStatusParams{Active: active})
		return c.delegate(ctx, method, params)
	default:
		return c.delegate(ctx, method, raw)
	}
}

func connectionLimitOccupied(snapshot lifecycle.Snapshot) bool {
	limit := snapshot.ConnectionLimit
	if limit <= 0 {
		limit = 1
	}
	return snapshot.TrustedPeers >= limit
}

func (c *DesktopController) forgetDeviceLocked(ctx context.Context, params localapi.ForgetDeviceParams) (any, error) {
	snapshot := c.supervisor.Snapshot()
	deviceID := strings.TrimSpace(params.DeviceID)
	if snapshot.Peer == nil || deviceID != "" && deviceID != snapshot.Peer.ID {
		return nil, needsAction("trusted device was not found")
	}
	if c.fallback == nil {
		return nil, unavailable("paired device cleanup is unavailable")
	}
	reserved, err := c.supervisor.machine.Apply(lifecycle.Event{
		Type: lifecycle.EventTrustForgetStarted,
		Peer: &lifecycle.Peer{ID: snapshot.Peer.ID},
	})
	if err != nil || reserved.Peer == nil {
		return nil, needsAction("disconnect before forgetting the trusted device")
	}
	rollback := func(cause error) (any, error) {
		if _, err := c.supervisor.machine.Apply(lifecycle.Event{Type: lifecycle.EventTrustForgetCancelled}); err != nil {
			return nil, unavailable("trusted-device cleanup reservation could not be released")
		}
		return nil, cause
	}
	unpairRaw, _ := json.Marshal(localapi.UnpairParams{DeviceID: reserved.Peer.ID, LocalOnly: params.LocalOnly})
	if _, err := c.fallback.Handle(ctx, localapi.MethodUnpair, unpairRaw); err != nil {
		return rollback(err)
	}
	if _, err := c.supervisor.machine.Apply(lifecycle.Event{Type: lifecycle.EventTrustForgotten}); err != nil {
		return rollback(unavailable("trusted-device cleanup could not be committed"))
	}
	return c.actionResult(), nil
}

func (c *DesktopController) replaceDeviceLocked(ctx context.Context, params localapi.ReplaceDeviceParams) (any, error) {
	oldDeviceID := strings.TrimSpace(params.OldDeviceID)
	newDevice := strings.TrimSpace(params.NewDevice)
	snapshot := c.supervisor.Snapshot()
	if snapshot.Role != lifecycle.RoleMacClient || snapshot.State != lifecycle.StateSearching ||
		snapshot.Pairing != nil || snapshot.Peer == nil || snapshot.TrustedPeers != 1 ||
		oldDeviceID == "" || snapshot.Peer.ID != oldDeviceID || newDevice == "" || newDevice == oldDeviceID {
		return nil, needsAction("device replacement requires active search and the exact saved device")
	}
	pairRaw, _ := json.Marshal(localapi.PairStartParams{Device: newDevice})
	result, err := c.delegate(ctx, localapi.MethodPairStart, pairRaw)
	if err != nil {
		return nil, err
	}
	started, ok := result.(localapi.PairStartResult)
	if !ok {
		return nil, unavailable("pairing returned an invalid response")
	}
	if _, err := pairingFromStart(started); err != nil {
		return c.rollbackStartedPairing(started, err)
	}
	if _, err := c.forgetDeviceLocked(ctx, localapi.ForgetDeviceParams{
		DeviceID: oldDeviceID, LocalOnly: params.LocalOnly,
	}); err != nil {
		return c.rollbackStartedPairing(started, err)
	}
	if err := c.startPairing(started); err != nil {
		return c.rollbackStartedPairing(started, err)
	}
	return started, nil
}

func (c *DesktopController) rollbackStartedPairing(started localapi.PairStartResult, cause error) (any, error) {
	cancelCtx, cancel := context.WithTimeout(context.Background(), pairingRollbackTimeout)
	defer cancel()
	cancelRaw, _ := json.Marshal(localapi.PairSessionParams{SessionID: started.SessionID})
	if _, err := c.delegate(cancelCtx, localapi.MethodPairCancel, cancelRaw); err != nil {
		if preserveErr := c.preserveRollbackPairing(started); preserveErr != nil {
			return nil, unavailable("pairing rollback was not confirmed; retry cancellation from the current pairing session")
		}
		return nil, unavailable("pairing cancellation was not confirmed; the request remains visible for retry")
	}
	return nil, cause
}

func (c *DesktopController) preserveRollbackPairing(started localapi.PairStartResult) error {
	expiresAt, err := time.Parse(time.RFC3339Nano, started.ExpiresAt)
	if err != nil {
		expiresAt = time.Now().Add(time.Minute)
	}
	pairing := lifecycle.Pairing{
		SessionID: started.SessionID, Peer: peerFromLocalAPI(started.Peer), Code: started.Code,
		Status: lifecycle.PairingPending, ExpiresAt: expiresAt,
	}
	eventType := lifecycle.EventPairingStarted
	if snapshot := c.supervisor.Snapshot(); snapshot.TrustedPeers == 1 && snapshot.Peer != nil {
		eventType = lifecycle.EventPairingCancellationPending
	}
	_, err = c.supervisor.machine.Apply(lifecycle.Event{Type: eventType, Pairing: &pairing})
	return err
}

func (c *DesktopController) startTrustedConnection(ctx context.Context, foreground func() (any, error)) (any, error) {
	if _, err := c.supervisor.machine.Apply(lifecycle.Event{Type: lifecycle.EventConnectionStartReserved}); err != nil {
		return nil, needsAction("trusted device connection cannot start in the current state")
	}
	abort := func(cause error) (any, error) {
		if err := c.supervisor.AbortConnectionStart(); err != nil {
			return nil, unavailable("failed connection startup could not be stopped safely")
		}
		return nil, cause
	}
	if err := c.supervisor.Start(ctx); err != nil {
		return abort(unavailable("Remote Docker could not be enabled"))
	}
	var result any
	if foreground != nil {
		var err error
		result, err = foreground()
		if err != nil {
			return abort(err)
		}
	}
	if _, err := c.supervisor.machine.Apply(lifecycle.Event{Type: lifecycle.EventConnectionStartCommitted}); err != nil {
		return abort(unavailable("trusted device connection could not be committed"))
	}
	return result, nil
}

func (c *DesktopController) startPairing(started localapi.PairStartResult) error {
	pairing, err := pairingFromStart(started)
	if err != nil {
		return err
	}
	if _, err := c.supervisor.machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingStarted, Pairing: &pairing}); err != nil {
		return needsAction("the device is not ready to start pairing")
	}
	return nil
}

func pairingFromStart(started localapi.PairStartResult) (lifecycle.Pairing, error) {
	expiresAt, err := time.Parse(time.RFC3339Nano, started.ExpiresAt)
	if err != nil || !expiresAt.After(time.Now()) || strings.TrimSpace(started.SessionID) == "" ||
		strings.TrimSpace(started.Peer.ID) == "" || !sixDigitPairCode(started.Code) {
		return lifecycle.Pairing{}, unavailable("pairing returned invalid session metadata")
	}
	return lifecycle.Pairing{
		SessionID: started.SessionID, Peer: peerFromLocalAPI(started.Peer), Code: started.Code,
		Status: lifecycle.PairingPending, ExpiresAt: expiresAt,
	}, nil
}

func sixDigitPairCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, digit := range code {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func (c *DesktopController) reconcilePairing(status localapi.PairingStatusResult) error {
	return reconcilePairingLifecycle(c.supervisor.machine, status)
}

func reconcilePairingLifecycle(machine *lifecycle.Machine, status localapi.PairingStatusResult) error {
	if machine == nil {
		return unavailable("pairing lifecycle is unavailable")
	}
	snapshot := machine.Snapshot()
	if snapshot.State == lifecycle.StatePairingCancellationPending {
		if snapshot.Pairing == nil || snapshot.Pairing.SessionID != status.SessionID {
			return needsAction("a different replacement cancellation is active")
		}
		switch status.Status {
		case string(pairing.SessionCancelled):
			_, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingCancelled})
			return err
		case string(lifecycle.PairingRejected):
			_, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingRejected})
			return err
		case string(lifecycle.PairingExpired):
			_, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingExpired})
			return err
		case string(lifecycle.PairingPending), string(lifecycle.PairingApproved), string(pairing.SessionCompleted):
			return nil
		default:
			return unavailable("replacement cancellation returned an invalid status")
		}
	}
	if snapshot.Pairing == nil {
		if status.Status != string(lifecycle.PairingPending) && status.Status != string(lifecycle.PairingApproved) {
			return nil
		}
		pairingState, err := pairingFromStart(localapi.PairStartResult{
			SessionID: status.SessionID, Code: status.Code, Peer: status.Peer, ExpiresAt: status.ExpiresAt,
		})
		if err != nil {
			return err
		}
		if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingStarted, Pairing: &pairingState}); err != nil {
			return needsAction("the device is not ready to start pairing")
		}
		snapshot = machine.Snapshot()
	}
	if snapshot.Pairing == nil || snapshot.Pairing.SessionID != status.SessionID {
		return needsAction("a different pairing request is active")
	}

	switch status.Status {
	case string(lifecycle.PairingPending):
		return nil
	case string(lifecycle.PairingApproved):
		if snapshot.Pairing.Status == lifecycle.PairingPending {
			_, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingApproved})
			return err
		}
		return nil
	case string(lifecycle.PairingRejected):
		_, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingRejected})
		return err
	case string(pairing.SessionCancelled):
		_, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingCancelled})
		return err
	case string(lifecycle.PairingExpired):
		_, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingExpired})
		return err
	case string(lifecycle.PairingCompleted):
		if status.Device == nil || strings.TrimSpace(status.Device.ID) == "" {
			return unavailable("completed pairing did not return a trusted device")
		}
		if snapshot.Pairing.Status == lifecycle.PairingPending {
			if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingApproved}); err != nil {
				return err
			}
		}
		peer := peerFromLocalAPI(status.Peer)
		peer.ID = status.Device.ID
		peer.Name = status.Device.Name
		peer.Address = status.Device.Address
		_, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingCompleted, Peer: &peer})
		return err
	default:
		return unavailable("pairing returned an unknown state")
	}
}

func peerFromLocalAPI(peer localapi.LifecyclePeer) lifecycle.Peer {
	return lifecycle.Peer{ID: peer.ID, Name: peer.Name, OS: peer.OS, Version: peer.Version, Address: peer.Address}
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
		Docker:    localapi.ServiceStatus{State: string(snapshot.Docker.State), Message: snapshot.Docker.Message},
		Sync:      localapi.ServiceStatus{State: string(snapshot.Sync.State), Pending: snapshot.Sync.Pending},
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
