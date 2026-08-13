package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
	"github.com/Dmitbd/remote-docker/internal/windowsbridge"
)

const connectionOperationTimeout = 5 * time.Second

type connectionSessionRuntime interface {
	Run(context.Context, time.Duration) error
	Stop(context.Context, lifecycle.StopReason) error
}

type presenceTransportFactory func(context.Context) (PresenceTransport, error)

type clientConnectionRuntime struct {
	machine        *lifecycle.Machine
	clientDeviceID func() string
	prepare        func(context.Context) error
	ready          func() bool
	localName      string
	appVersion     string
	transport      presenceTransportFactory

	mu              sync.Mutex
	lifetimeCtx     context.Context
	active          *ClientPresence
	activeTransport PresenceTransport
	prepared        bool
}

func (r *clientConnectionRuntime) Run(ctx context.Context, interval time.Duration) error {
	if r == nil || r.machine == nil || r.transport == nil || r.clientDeviceID == nil || r.ready == nil {
		return errors.New("client connection runtime is incomplete")
	}
	if interval <= 0 {
		interval = time.Second
	}
	r.mu.Lock()
	r.lifetimeCtx = ctx
	r.prepared = false
	r.mu.Unlock()
	r.stepBounded(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.closeActive()
			return ctx.Err()
		case <-ticker.C:
			r.stepBounded(ctx)
		}
	}
}

func (r *clientConnectionRuntime) stepBounded(ctx context.Context) {
	stepCtx, cancel := context.WithTimeout(ctx, connectionOperationTimeout)
	defer cancel()
	_ = r.step(stepCtx)
}

func (r *clientConnectionRuntime) step(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot := r.machine.Snapshot()
	if snapshot.TrustedPeers != 1 || snapshot.Peer == nil || snapshot.State == lifecycle.StatePaused ||
		snapshot.State == lifecycle.StateStopping || snapshot.State == lifecycle.StateNeedsAction || snapshot.Terminal {
		return nil
	}
	if !r.prepared && r.prepare != nil {
		if err := r.prepare(ctx); err != nil {
			return err
		}
		r.prepared = true
	}
	if !r.ready() {
		if snapshot.State == lifecycle.StateConnected {
			_, _ = r.machine.Apply(lifecycle.Event{Type: lifecycle.EventNetworkLost})
		}
		return nil
	}
	if r.active != nil {
		if err := r.active.Heartbeat(ctx); err != nil {
			r.closeActiveLocked()
		}
		return nil
	}
	deviceID := strings.TrimSpace(r.clientDeviceID())
	if deviceID == "" {
		return nil
	}
	processCtx := r.lifetimeCtx
	if processCtx == nil {
		processCtx = ctx
	}
	transport, err := r.transport(processCtx)
	if err != nil || transport == nil {
		return nil
	}
	presence, err := NewClientPresence(r.machine, transport, time.Now, func(context.Context) error { return nil })
	if err != nil {
		closePresenceTransport(transport)
		return nil
	}
	if err := presence.Start(ctx, deviceID, r.localName, r.appVersion); err != nil {
		closePresenceTransport(transport)
		return nil
	}
	r.active = presence
	r.activeTransport = transport
	return nil
}

func (r *clientConnectionRuntime) Stop(ctx context.Context, reason lifecycle.StopReason) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var notifyErr error
	if r.active != nil {
		notifyErr = r.active.NotifyStop(ctx, connectionReasonForStop(reason))
	}
	r.closeActiveLocked()
	return notifyErr
}

func (r *clientConnectionRuntime) closeActive() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeActiveLocked()
}

func (r *clientConnectionRuntime) closeActiveLocked() {
	closePresenceTransport(r.activeTransport)
	r.active = nil
	r.activeTransport = nil
}

func closePresenceTransport(transport PresenceTransport) {
	if closer, ok := transport.(io.Closer); ok {
		_ = closer.Close()
	}
}

func connectionReasonForStop(reason lifecycle.StopReason) lifecycle.ConnectionReason {
	switch reason {
	case lifecycle.StopPause:
		return lifecycle.ReasonUserPause
	case lifecycle.StopQuit:
		return lifecycle.ReasonPeerQuit
	case lifecycle.StopFailure:
		return lifecycle.ReasonRuntimeFailure
	default:
		return lifecycle.ReasonUserDisconnect
	}
}

type managedPresenceObserver interface {
	Observe(context.Context) (windowsbridge.ManagedWSLStatus, error)
}

type hostConnectionRuntime struct {
	machine  *lifecycle.Machine
	observer managedPresenceObserver
	now      func() time.Time
	presence *HostPresence
	active   bool
	sequence uint64
}

func newHostConnectionRuntime(machine *lifecycle.Machine, observer managedPresenceObserver, now func() time.Time) (*hostConnectionRuntime, error) {
	if now == nil {
		now = time.Now
	}
	presence, err := NewHostPresence(machine, now, func(context.Context) error { return nil })
	if err != nil {
		return nil, err
	}
	if observer == nil {
		return nil, errors.New("host presence observer is unavailable")
	}
	return &hostConnectionRuntime{machine: machine, observer: observer, now: now, presence: presence}, nil
}

func (r *hostConnectionRuntime) Run(ctx context.Context, interval time.Duration) error {
	if r == nil || r.machine == nil || r.presence == nil || r.observer == nil {
		return errors.New("host connection runtime is incomplete")
	}
	if interval <= 0 {
		interval = time.Second
	}
	r.stepBounded(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.stepBounded(ctx)
		}
	}
}

func (r *hostConnectionRuntime) stepBounded(ctx context.Context) {
	stepCtx, cancel := context.WithTimeout(ctx, connectionOperationTimeout)
	defer cancel()
	_ = r.step(stepCtx)
}

func (r *hostConnectionRuntime) step(ctx context.Context) error {
	snapshot := r.machine.Snapshot()
	if snapshot.State == lifecycle.StatePaused || snapshot.State == lifecycle.StateStopping || snapshot.State == lifecycle.StateNeedsAction || snapshot.Terminal {
		return nil
	}
	status, err := r.observer.Observe(ctx)
	ready := err == nil && status.Running && status.DockerSocket && status.SyncthingService && status.PresenceActive
	if ready && !r.active {
		if snapshot.Peer == nil {
			return nil
		}
		if err := r.presence.Hello("authenticated-ssh-presence", *snapshot.Peer); err == nil {
			r.active = true
			r.sequence = 0
			return nil
		}
	}
	if ready && r.active {
		r.sequence++
		if err := r.presence.Heartbeat("authenticated-ssh-presence", r.sequence, 0); err != nil {
			r.active = false
			r.sequence = 0
		}
		return nil
	}
	if tickErr := r.presence.Tick(ctx); tickErr != nil {
		return tickErr
	}
	if r.machine.Snapshot().State == lifecycle.StateHostWaiting {
		r.active = false
		r.sequence = 0
	}
	return nil
}

func (r *hostConnectionRuntime) Stop(context.Context, lifecycle.StopReason) error {
	return nil
}

var _ connectionSessionRuntime = (*clientConnectionRuntime)(nil)
var _ connectionSessionRuntime = (*hostConnectionRuntime)(nil)
