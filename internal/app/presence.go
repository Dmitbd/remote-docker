package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
)

const presenceHeartbeatTimeout = 5 * time.Second

var (
	ErrPresenceLease    = errors.New("presence lease is unavailable")
	ErrPresenceSequence = errors.New("presence heartbeat sequence is invalid")
)

type PresenceHello struct {
	ClientDeviceID string
	ClientName     string
	AppVersion     string
}

type PresenceHelloResult struct {
	SessionID   string
	DockerReady bool
	SyncReady   bool
}

type PresenceHeartbeatResult struct {
	DockerReady bool
	SyncReady   bool
	Terminal    bool
	Reason      string
}

type PresenceTransport interface {
	Hello(context.Context, PresenceHello) (PresenceHelloResult, error)
	Heartbeat(context.Context, string, uint64) (PresenceHeartbeatResult, error)
	Disconnect(context.Context, string, string) error
}

type ClientPresence struct {
	machine   *lifecycle.Machine
	transport PresenceTransport
	now       func() time.Time
	cleanup   func(context.Context) error

	mu        sync.Mutex
	sessionID string
	sequence  uint64
}

func NewClientPresence(machine *lifecycle.Machine, transport PresenceTransport, now func() time.Time, cleanup func(context.Context) error) (*ClientPresence, error) {
	if machine == nil || transport == nil || cleanup == nil || machine.Snapshot().Role != lifecycle.RoleMacClient {
		return nil, errors.New("client presence dependencies are incomplete")
	}
	if now == nil {
		now = time.Now
	}
	return &ClientPresence{machine: machine, transport: transport, now: now, cleanup: cleanup}, nil
}

func (p *ClientPresence) Start(ctx context.Context, clientDeviceID, clientName, appVersion string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessionID != "" {
		return ErrPresenceLease
	}
	result, err := p.transport.Hello(ctx, PresenceHello{
		ClientDeviceID: clientDeviceID, ClientName: clientName, AppVersion: appVersion,
	})
	if err != nil || strings.TrimSpace(result.SessionID) == "" {
		return ErrPresenceLease
	}
	snapshot := p.machine.Snapshot()
	if snapshot.State != lifecycle.StateConnecting && snapshot.State != lifecycle.StateReconnecting {
		if _, err := p.machine.Apply(lifecycle.Event{Type: lifecycle.EventConnectionStarted}); err != nil {
			_ = p.transport.Disconnect(ctx, result.SessionID, string(lifecycle.ReasonRuntimeFailure))
			return err
		}
	}
	if result.DockerReady && result.SyncReady {
		event := lifecycle.Event{Type: lifecycle.EventConnected}
		if snapshot.State == lifecycle.StateReconnecting {
			event.Type = lifecycle.EventNetworkRestored
		}
		if _, err := p.machine.Apply(event); err != nil {
			_ = p.transport.Disconnect(ctx, result.SessionID, string(lifecycle.ReasonRuntimeFailure))
			return err
		}
	}
	p.sessionID = result.SessionID
	p.sequence = 0
	return nil
}

func (p *ClientPresence) Heartbeat(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessionID == "" {
		return ErrPresenceLease
	}
	p.sequence++
	started := p.now()
	result, err := p.transport.Heartbeat(ctx, p.sessionID, p.sequence)
	latency := p.now().Sub(started)
	if err != nil {
		if p.machine.Snapshot().State == lifecycle.StateConnected {
			_, _ = p.machine.Apply(lifecycle.Event{Type: lifecycle.EventNetworkLost})
		}
		return err
	}
	if result.Terminal {
		return p.finishLocked(ctx, lifecycle.InitiatorPeer, lifecycle.ReasonUserDisconnect)
	}
	snapshot := p.machine.Snapshot()
	ready := result.DockerReady && result.SyncReady
	switch {
	case ready && snapshot.State == lifecycle.StateConnecting:
		_, err = p.machine.Apply(lifecycle.Event{Type: lifecycle.EventConnected, Latency: latency})
	case ready && snapshot.State == lifecycle.StateReconnecting:
		_, err = p.machine.Apply(lifecycle.Event{Type: lifecycle.EventNetworkRestored, Latency: latency})
	case ready && snapshot.State == lifecycle.StateConnected:
		_, err = p.machine.Apply(lifecycle.Event{Type: lifecycle.EventHeartbeat, Latency: latency})
	case !ready && snapshot.State == lifecycle.StateConnected:
		_, err = p.machine.Apply(lifecycle.Event{Type: lifecycle.EventNetworkLost})
	}
	return err
}

func (p *ClientPresence) Disconnect(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessionID == "" {
		return ErrPresenceLease
	}
	if err := p.transport.Disconnect(ctx, p.sessionID, string(lifecycle.ReasonUserDisconnect)); err != nil {
		return err
	}
	return p.finishLocked(ctx, lifecycle.InitiatorLocal, lifecycle.ReasonUserDisconnect)
}

// NotifyStop closes the authenticated remote lease while Supervisor owns the
// local lifecycle transition. It deliberately does not apply a second stop
// event to the state machine.
func (p *ClientPresence) NotifyStop(ctx context.Context, reason lifecycle.ConnectionReason) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessionID == "" {
		return nil
	}
	err := p.transport.Disconnect(ctx, p.sessionID, string(reason))
	p.sessionID = ""
	p.sequence = 0
	return err
}

func (p *ClientPresence) finishLocked(ctx context.Context, initiator lifecycle.Initiator, reason lifecycle.ConnectionReason) error {
	if _, err := p.machine.Apply(lifecycle.Event{Type: lifecycle.EventDisconnectRequested, Disconnect: &lifecycle.Disconnect{
		Initiator: initiator, Reason: reason,
	}}); err != nil {
		return err
	}
	if err := p.cleanup(ctx); err != nil {
		return err
	}
	if _, err := p.machine.Apply(lifecycle.Event{Type: lifecycle.EventStopCompleted}); err != nil {
		return err
	}
	p.sessionID = ""
	p.sequence = 0
	return nil
}

type HostPresence struct {
	machine *lifecycle.Machine
	now     func() time.Time
	cleanup func(context.Context) error

	mu          sync.Mutex
	sessionID   string
	peerID      string
	sequence    uint64
	lastSeen    time.Time
	cleanupDone bool
}

func NewHostPresence(machine *lifecycle.Machine, now func() time.Time, cleanup func(context.Context) error) (*HostPresence, error) {
	if machine == nil || cleanup == nil {
		return nil, errors.New("host presence dependencies are incomplete")
	}
	if machine.Snapshot().Role != lifecycle.RoleWindowsHost {
		return nil, errors.New("host presence requires the Windows role")
	}
	if now == nil {
		now = time.Now
	}
	return &HostPresence{machine: machine, now: now, cleanup: cleanup}, nil
}

func (p *HostPresence) Hello(sessionID string, peer lifecycle.Peer) error {
	sessionID = strings.TrimSpace(sessionID)
	peer.ID = strings.TrimSpace(peer.ID)
	if sessionID == "" || peer.ID == "" {
		return ErrPresenceLease
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessionID != "" {
		return ErrPresenceLease
	}
	snapshot := p.machine.Snapshot()
	if snapshot.Peer == nil || snapshot.TrustedPeers != 1 || snapshot.Peer.ID != peer.ID ||
		(snapshot.State != lifecycle.StateHostWaiting && snapshot.State != lifecycle.StateConnecting) {
		return ErrPresenceLease
	}
	if snapshot.State == lifecycle.StateHostWaiting {
		if _, err := p.machine.Apply(lifecycle.Event{Type: lifecycle.EventConnectionStarted}); err != nil {
			return ErrPresenceLease
		}
	}
	if _, err := p.machine.Apply(lifecycle.Event{Type: lifecycle.EventConnected}); err != nil {
		return ErrPresenceLease
	}
	p.sessionID = sessionID
	p.peerID = peer.ID
	p.sequence = 0
	p.lastSeen = p.now()
	p.cleanupDone = false
	return nil
}

func (p *HostPresence) Heartbeat(sessionID string, sequence uint64, latency time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessionID == "" || sessionID != p.sessionID {
		return ErrPresenceLease
	}
	if sequence <= p.sequence {
		return ErrPresenceSequence
	}
	p.sequence = sequence
	p.lastSeen = p.now()
	snapshot := p.machine.Snapshot()
	if snapshot.State == lifecycle.StateReconnecting {
		_, err := p.machine.Apply(lifecycle.Event{Type: lifecycle.EventNetworkRestored, Latency: latency})
		return err
	}
	if snapshot.State != lifecycle.StateConnected {
		return ErrPresenceLease
	}
	_, err := p.machine.Apply(lifecycle.Event{Type: lifecycle.EventHeartbeat, Latency: latency})
	return err
}

func (p *HostPresence) Tick(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessionID == "" {
		return nil
	}
	snapshot := p.machine.Snapshot()
	if snapshot.State == lifecycle.StateConnected && p.now().Sub(p.lastSeen) >= presenceHeartbeatTimeout {
		_, err := p.machine.Apply(lifecycle.Event{Type: lifecycle.EventNetworkLost})
		return err
	}
	if snapshot.State != lifecycle.StateReconnecting || snapshot.Recovery == nil || p.now().Before(snapshot.Recovery.Deadline) {
		return nil
	}
	if p.cleanupDone {
		return nil
	}
	p.cleanupDone = true
	if _, err := p.machine.Apply(lifecycle.Event{Type: lifecycle.EventRecoveryExpired}); err != nil {
		return err
	}
	if err := p.cleanup(ctx); err != nil {
		_, _ = p.machine.Apply(lifecycle.Event{Type: lifecycle.EventProblemDetected, Problem: &lifecycle.Problem{
			Code: "network_cleanup_failed", Device: lifecycle.InitiatorLocal,
			Message: "Remote Docker could not stop cleanly after the connection was lost.",
			Action:  "Open diagnostics and stop Remote Docker again.",
		}})
		return err
	}
	if _, err := p.machine.Apply(lifecycle.Event{Type: lifecycle.EventStopCompleted}); err != nil {
		return err
	}
	p.clearLocked()
	return nil
}

func (p *HostPresence) Disconnect(ctx context.Context, initiator lifecycle.Initiator, reason lifecycle.ConnectionReason) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessionID == "" {
		return ErrPresenceLease
	}
	if _, err := p.machine.Apply(lifecycle.Event{Type: lifecycle.EventDisconnectRequested, Disconnect: &lifecycle.Disconnect{
		Initiator: initiator, Reason: reason,
	}}); err != nil {
		return err
	}
	if !p.cleanupDone {
		p.cleanupDone = true
		if err := p.cleanup(ctx); err != nil {
			return err
		}
	}
	if _, err := p.machine.Apply(lifecycle.Event{Type: lifecycle.EventStopCompleted}); err != nil {
		return err
	}
	p.clearLocked()
	return nil
}

func (p *HostPresence) clearLocked() {
	p.sessionID = ""
	p.peerID = ""
	p.sequence = 0
	p.lastSeen = time.Time{}
}
