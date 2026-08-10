package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/Dmitbd/remote-docker/internal/localapi"
)

type AgentState string

const (
	AgentUnpaired       AgentState = "Unpaired"
	AgentConnecting     AgentState = "Connecting"
	AgentEngineStarting AgentState = "EngineStarting"
	AgentSyncing        AgentState = "Syncing"
	AgentReady          AgentState = "Ready"
	AgentDegraded       AgentState = "Degraded"
	AgentNeedsAction    AgentState = "NeedsAction"
)

type AgentStatus struct {
	State   AgentState `json:"state"`
	Paired  bool       `json:"paired"`
	Message string     `json:"message,omitempty"`
}

type AgentObservation struct {
	Paired             bool
	PinnedSSH          bool
	DockerPing         bool
	SyncthingConnected bool
	NeedsAction        string
	Err                error
}

type AgentObserver interface {
	Observe(context.Context) AgentObservation
}

type ObservationFunc func(context.Context) AgentObservation

func (f ObservationFunc) Observe(ctx context.Context) AgentObservation { return f(ctx) }

type InfrastructureRestorer interface {
	RestoreEventStream(context.Context) error
	RestoreRelays(context.Context) error
}

type AgentController interface {
	Handle(context.Context, localapi.Method, json.RawMessage) (any, error)
}

type Agent struct {
	observer   AgentObserver
	restorer   InfrastructureRestorer
	controller AgentController

	mu     sync.RWMutex
	status AgentStatus
}

func NewAgent(observer AgentObserver, restorer InfrastructureRestorer, controller AgentController) *Agent {
	return &Agent{
		observer: observer, restorer: restorer, controller: controller,
		status: AgentStatus{State: AgentUnpaired},
	}
}

func (a *Agent) abandonPairing(sessionID string) {
	if a == nil || a.controller == nil {
		return
	}
	if cleaner, ok := a.controller.(interface{ abandonPairing(string) }); ok {
		cleaner.abandonPairing(sessionID)
	}
}

func (a *Agent) Status() AgentStatus {
	if a == nil {
		return AgentStatus{State: AgentNeedsAction, Message: "background agent is unavailable"}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.status
}

func (a *Agent) Refresh(ctx context.Context) AgentStatus {
	if a == nil {
		return AgentStatus{State: AgentNeedsAction, Message: "background agent is unavailable"}
	}
	observation := AgentObservation{}
	if a.observer != nil {
		observation = a.observer.Observe(ctx)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	previous := a.status
	a.status = evaluateAgent(previous, observation)
	return a.status
}

func evaluateAgent(previous AgentStatus, observation AgentObservation) AgentStatus {
	if observation.NeedsAction != "" {
		return AgentStatus{State: AgentNeedsAction, Paired: observation.Paired, Message: observation.NeedsAction}
	}
	if observation.Err != nil {
		paired := observation.Paired || previous.Paired
		if previous.State == AgentReady || previous.State == AgentDegraded {
			return AgentStatus{State: AgentDegraded, Paired: paired, Message: "connection health check failed"}
		}
		return AgentStatus{State: AgentNeedsAction, Paired: paired, Message: "background agent health check failed"}
	}
	if !observation.Paired {
		return AgentStatus{State: AgentUnpaired, Message: "pair a device to continue"}
	}
	if !observation.PinnedSSH {
		return AgentStatus{State: AgentConnecting, Paired: true, Message: "establishing pinned SSH connection"}
	}
	if !observation.DockerPing {
		return AgentStatus{State: AgentEngineStarting, Paired: true, Message: "waiting for Docker Engine"}
	}
	if !observation.SyncthingConnected {
		return AgentStatus{State: AgentSyncing, Paired: true, Message: "waiting for Syncthing connection"}
	}
	return AgentStatus{State: AgentReady, Paired: true, Message: "connected"}
}

func (a *Agent) Reconnect(ctx context.Context) error {
	if a == nil || a.restorer == nil {
		return &localapi.PublicError{Code: localapi.ErrorUnavailable, Message: "recovery infrastructure is unavailable"}
	}
	// Persisted pairing is authoritative even while its transport is still
	// recovering. Publish that state before any fallible infrastructure work.
	a.Refresh(ctx)
	if err := a.restorer.RestoreEventStream(ctx); err != nil {
		return err
	}
	if err := a.restorer.RestoreRelays(ctx); err != nil {
		return err
	}
	status := a.Refresh(ctx)
	if status.State == AgentNeedsAction {
		return &localapi.PublicError{Code: localapi.ErrorNeedsAction, Message: status.Message}
	}
	return nil
}

func (a *Agent) Run(ctx context.Context, interval time.Duration) error {
	if a == nil {
		return errors.New("background agent is nil")
	}
	if interval <= 0 {
		interval = time.Second
	}
	a.Refresh(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			a.Refresh(ctx)
		}
	}
}

func (a *Agent) Handle(ctx context.Context, method localapi.Method, params json.RawMessage) (any, error) {
	if method == localapi.MethodStatus {
		status := a.Status()
		return localapi.StatusResult{State: string(status.State), Paired: status.Paired, Message: status.Message}, nil
	}
	if method == localapi.MethodConnect {
		return nil, a.Reconnect(ctx)
	}
	if a == nil || a.controller == nil {
		return nil, &localapi.PublicError{Code: localapi.ErrorUnavailable, Message: "agent operation is unavailable"}
	}
	return a.controller.Handle(ctx, method, params)
}

var _ localapi.Handler = (*Agent)(nil)
