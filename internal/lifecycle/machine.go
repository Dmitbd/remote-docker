package lifecycle

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const recoveryWindow = 60 * time.Second

type Command string

const (
	CommandEnable      Command = "enable"
	CommandStartSearch Command = "start_search"
	CommandStopSearch  Command = "stop_search"
	CommandConnect     Command = "connect"
	CommandApprove     Command = "approve"
	CommandReject      Command = "reject"
	CommandCancel      Command = "cancel"
	CommandDisconnect  Command = "disconnect"
	CommandPause       Command = "pause"
	CommandForget      Command = "forget"
	CommandQuit        Command = "quit"
)

type EventType string

const (
	EventEnabled                       EventType = "enabled"
	EventSearchStarted                 EventType = "search_started"
	EventSearchStopped                 EventType = "search_stopped"
	EventPairingStarted                EventType = "pairing_started"
	EventPairingApproved               EventType = "pairing_approved"
	EventPairingRejected               EventType = "pairing_rejected"
	EventPairingCancelled              EventType = "pairing_cancelled"
	EventPairingExpired                EventType = "pairing_expired"
	EventPairingCompleted              EventType = "pairing_completed"
	EventConnectionStarted             EventType = "connection_started"
	EventConnectionStartReserved       EventType = "connection_start_reserved"
	EventConnectionStartCommitted      EventType = "connection_start_committed"
	EventConnectionStartAbortRequested EventType = "connection_start_abort_requested"
	EventConnected                     EventType = "connected"
	EventHeartbeat                     EventType = "heartbeat"
	EventDisconnectRequested           EventType = "disconnect_requested"
	EventNetworkLost                   EventType = "network_lost"
	EventNetworkRestored               EventType = "network_restored"
	EventRecoveryExpired               EventType = "recovery_expired"
	EventPauseRequested                EventType = "pause_requested"
	EventQuitRequested                 EventType = "quit_requested"
	EventStopCompleted                 EventType = "stop_completed"
	EventTrustForgetStarted            EventType = "trust_forget_started"
	EventTrustForgotten                EventType = "trust_forgotten"
	EventTrustForgetCancelled          EventType = "trust_forget_cancelled"
	EventProblemDetected               EventType = "problem_detected"
	EventProblemCleared                EventType = "problem_cleared"
)

type Event struct {
	Type       EventType
	Pairing    *Pairing
	Peer       *Peer
	Disconnect *Disconnect
	Problem    *Problem
	Latency    time.Duration
}

type TransitionError struct {
	State State
	Event EventType
	Cause string
}

func (e *TransitionError) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause == "" {
		return fmt.Sprintf("lifecycle event %q is not allowed from %q", e.Event, e.State)
	}
	return fmt.Sprintf("lifecycle event %q is not allowed from %q: %s", e.Event, e.State, e.Cause)
}

type Option func(*Machine)

func WithClock(now func() time.Time) Option {
	return func(machine *Machine) {
		if now != nil {
			machine.now = now
		}
	}
}

// WithTrustedPeer restores the one public trusted peer without starting any
// runtime work. Construction still begins paused.
func WithTrustedPeer(peer Peer) Option {
	return func(machine *Machine) {
		if strings.TrimSpace(peer.ID) == "" || strings.TrimSpace(peer.Name) == "" {
			return
		}
		trusted := peer
		machine.snapshot.Peer = &trusted
		machine.snapshot.TrustedPeers = 1
	}
}

type Machine struct {
	mu                  sync.RWMutex
	snapshot            Snapshot
	now                 func() time.Time
	afterStop           State
	forgetting          bool
	connectionStarting  bool
	connectionStartFrom State
	problemFrom         State
	subscribers         map[uint64]chan Snapshot
	nextID              uint64
}

func NewMachine(role Role, localName string, options ...Option) (*Machine, error) {
	if role != RoleMacClient && role != RoleWindowsHost {
		return nil, errors.New("lifecycle role is invalid")
	}
	localName = strings.TrimSpace(localName)
	if localName == "" {
		return nil, errors.New("local device name is required")
	}
	machine := &Machine{
		now: time.Now,
		snapshot: Snapshot{
			Role: role, State: StatePaused, LocalName: localName, ConnectionLimit: 1,
			Docker: DockerStatus{State: ServiceStopped}, Sync: SyncStatus{State: ServiceStopped},
		},
		subscribers: make(map[uint64]chan Snapshot),
	}
	for _, option := range options {
		option(machine)
	}
	return machine, nil
}

func (m *Machine) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{State: StateNeedsAction, ConnectionLimit: 1}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshot.Clone()
}

func (m *Machine) Allowed(command Command) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.forgetting || m.connectionStarting {
		return false
	}
	return allowed(m.snapshot, command)
}

func allowed(snapshot Snapshot, command Command) bool {
	if snapshot.Terminal || snapshot.State == StateStopping {
		return false
	}
	switch command {
	case CommandEnable:
		return snapshot.State == StatePaused
	case CommandStartSearch:
		return snapshot.Role == RoleMacClient && snapshot.State == StateClientReady
	case CommandStopSearch:
		return snapshot.Role == RoleMacClient && snapshot.State == StateSearching
	case CommandConnect:
		return snapshot.Role == RoleMacClient && snapshot.TrustedPeers == 1 &&
			(snapshot.State == StateClientReady || snapshot.State == StateSearching)
	case CommandApprove, CommandReject:
		return snapshot.Role == RoleWindowsHost && snapshot.State == StatePairing &&
			snapshot.Pairing != nil && snapshot.Pairing.Status == PairingPending
	case CommandCancel:
		return snapshot.Role == RoleMacClient && snapshot.State == StatePairing
	case CommandDisconnect:
		return snapshot.State == StateConnected || snapshot.State == StateReconnecting
	case CommandPause:
		return snapshot.State != StatePaused
	case CommandForget:
		return snapshot.TrustedPeers == 1 && snapshot.Peer != nil &&
			(snapshot.State == StatePaused || snapshot.State == StateClientReady || snapshot.State == StateSearching ||
				snapshot.State == StateHostWaiting || snapshot.State == StateNeedsAction)
	case CommandQuit:
		return true
	default:
		return false
	}
}

func (m *Machine) Apply(event Event) (Snapshot, error) {
	if m == nil {
		return Snapshot{}, errors.New("lifecycle machine is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.snapshot.Terminal && event.Type != EventStopCompleted {
		return m.snapshot.Clone(), m.transitionError(event, "application shutdown is already in progress")
	}
	if err := m.applyLocked(event); err != nil {
		return m.snapshot.Clone(), err
	}
	m.snapshot.Revision++
	current := m.snapshot.Clone()
	for _, updates := range m.subscribers {
		select {
		case updates <- current.Clone():
		default:
			select {
			case <-updates:
			default:
			}
			select {
			case updates <- current.Clone():
			default:
			}
		}
	}
	return current, nil
}

func (m *Machine) applyLocked(event Event) error {
	snapshot := &m.snapshot
	if m.forgetting && event.Type != EventTrustForgotten && event.Type != EventTrustForgetCancelled {
		return m.transitionError(event, "trusted-device cleanup is in progress")
	}
	if m.connectionStarting {
		switch event.Type {
		case EventConnectionStartCommitted, EventConnectionStartAbortRequested, EventConnected,
			EventHeartbeat, EventNetworkLost, EventNetworkRestored, EventStopCompleted:
		default:
			return m.transitionError(event, "trusted connection startup is in progress")
		}
	}
	switch event.Type {
	case EventEnabled:
		if snapshot.State != StatePaused {
			return m.transitionError(event, "application is not paused")
		}
		snapshot.State = m.idleState()
		snapshot.Terminal = false
		snapshot.Problem = nil
		m.problemFrom = ""
	case EventSearchStarted:
		if snapshot.Role != RoleMacClient || snapshot.State != StateClientReady {
			return m.transitionError(event, "only an enabled Mac client can search")
		}
		snapshot.State = StateSearching
	case EventSearchStopped:
		if snapshot.Role != RoleMacClient || snapshot.State != StateSearching {
			return m.transitionError(event, "search is not active")
		}
		snapshot.State = StateClientReady
	case EventPairingStarted:
		if event.Pairing == nil || event.Pairing.SessionID == "" || event.Pairing.Peer.ID == "" || !sixDigits(event.Pairing.Code) {
			return m.transitionError(event, "pairing metadata is incomplete")
		}
		if snapshot.TrustedPeers != 0 || snapshot.Pairing != nil {
			return m.transitionError(event, "one trusted device is already present")
		}
		validStart := snapshot.Role == RoleMacClient && snapshot.State == StateSearching ||
			snapshot.Role == RoleWindowsHost && snapshot.State == StateHostWaiting ||
			snapshot.State == StateNeedsAction && snapshot.Problem != nil &&
				(snapshot.Role == RoleMacClient && m.problemFrom == StateSearching ||
					snapshot.Role == RoleWindowsHost && m.problemFrom == StateHostWaiting)
		if !validStart {
			return m.transitionError(event, "device is not accepting a pairing request")
		}
		pairing := *event.Pairing
		pairing.Status = PairingPending
		snapshot.Pairing = &pairing
		snapshot.State = StatePairing
	case EventPairingApproved:
		if snapshot.State != StatePairing || snapshot.Pairing == nil || snapshot.Pairing.Status != PairingPending {
			return m.transitionError(event, "no pending pairing request exists")
		}
		pairing := *snapshot.Pairing
		pairing.Status = PairingApproved
		snapshot.Pairing = &pairing
	case EventPairingRejected, EventPairingCancelled, EventPairingExpired:
		if snapshot.State != StatePairing || snapshot.Pairing == nil {
			return m.transitionError(event, "no pairing request exists")
		}
		snapshot.Pairing = nil
		if snapshot.Problem != nil {
			snapshot.State = StateNeedsAction
		} else {
			snapshot.State = m.idleState()
			m.problemFrom = ""
		}
	case EventPairingCompleted:
		if snapshot.State != StatePairing || snapshot.Pairing == nil || snapshot.Pairing.Status != PairingApproved || event.Peer == nil || event.Peer.ID == "" {
			return m.transitionError(event, "approved pairing metadata is unavailable")
		}
		peer := *event.Peer
		snapshot.Peer = &peer
		snapshot.TrustedPeers = 1
		snapshot.Pairing = nil
		snapshot.Problem = nil
		m.problemFrom = ""
		snapshot.State = StateConnecting
		snapshot.Docker.State = ServiceStarting
		snapshot.Sync.State = ServiceStarting
	case EventConnectionStarted:
		validStart := snapshot.TrustedPeers == 1 && snapshot.Peer != nil &&
			(snapshot.Role == RoleMacClient && (snapshot.State == StateClientReady || snapshot.State == StateSearching) ||
				snapshot.Role == RoleWindowsHost && snapshot.State == StateHostWaiting)
		if !validStart {
			return m.transitionError(event, "trusted peer is unavailable")
		}
		snapshot.State = StateConnecting
		snapshot.Docker.State = ServiceStarting
		snapshot.Sync.State = ServiceStarting
	case EventConnectionStartReserved:
		validStart := snapshot.TrustedPeers == 1 && snapshot.Peer != nil &&
			(snapshot.State == StatePaused ||
				snapshot.Role == RoleMacClient && (snapshot.State == StateClientReady || snapshot.State == StateSearching) ||
				snapshot.Role == RoleWindowsHost && snapshot.State == StateHostWaiting)
		if !validStart || m.connectionStarting {
			return m.transitionError(event, "trusted connection cannot start in the current state")
		}
		m.connectionStarting = true
		m.connectionStartFrom = snapshot.State
		snapshot.State = StateConnecting
		snapshot.ActionInProgress = true
		snapshot.Docker.State = ServiceStarting
		snapshot.Sync.State = ServiceStarting
	case EventConnectionStartCommitted:
		if !m.connectionStarting ||
			(snapshot.State != StateConnecting && snapshot.State != StateConnected && snapshot.State != StateReconnecting) {
			return m.transitionError(event, "trusted connection startup is not reserved")
		}
		m.connectionStarting = false
		m.connectionStartFrom = ""
		snapshot.ActionInProgress = false
	case EventConnectionStartAbortRequested:
		if !m.connectionStarting ||
			(snapshot.State != StateConnecting && snapshot.State != StateConnected && snapshot.State != StateReconnecting) {
			return m.transitionError(event, "trusted connection startup is not reserved")
		}
		target := m.idleState()
		if m.connectionStartFrom == StatePaused {
			target = StatePaused
		}
		m.beginStop(&Disconnect{Initiator: InitiatorSystem, Reason: ReasonRuntimeFailure}, target)
	case EventConnected:
		if snapshot.State != StateConnecting || snapshot.TrustedPeers != 1 || snapshot.Peer == nil {
			return m.transitionError(event, "trusted connection is not being established")
		}
		snapshot.State = StateConnected
		snapshot.Latency = event.Latency
		snapshot.Docker.State = ServiceReady
		snapshot.Sync.State = ServiceReady
		snapshot.Recovery = nil
		snapshot.LastDisconnect = nil
	case EventHeartbeat:
		if snapshot.State != StateConnected {
			return m.transitionError(event, "connection is not active")
		}
		snapshot.Latency = event.Latency
	case EventDisconnectRequested:
		if snapshot.State != StateConnected && snapshot.State != StateReconnecting {
			return m.transitionError(event, "connection is not active")
		}
		m.beginStop(event.Disconnect, m.idleState())
	case EventNetworkLost:
		if snapshot.State != StateConnected {
			return m.transitionError(event, "connection is not active")
		}
		snapshot.State = StateReconnecting
		snapshot.Recovery = &Recovery{Deadline: m.now().Add(recoveryWindow)}
	case EventNetworkRestored:
		if snapshot.State != StateReconnecting {
			return m.transitionError(event, "connection is not recovering")
		}
		snapshot.State = StateConnected
		snapshot.Recovery = nil
		snapshot.Latency = event.Latency
	case EventRecoveryExpired:
		if snapshot.State != StateReconnecting {
			return m.transitionError(event, "connection recovery is not active")
		}
		m.beginStop(&Disconnect{Initiator: InitiatorSystem, Reason: ReasonNetworkTimeout}, m.idleState())
	case EventPauseRequested:
		if snapshot.State == StatePaused || snapshot.State == StateStopping {
			return m.transitionError(event, "application is already paused or stopping")
		}
		m.beginStop(&Disconnect{Initiator: InitiatorLocal, Reason: ReasonUserPause}, StatePaused)
	case EventQuitRequested:
		if snapshot.State == StateStopping {
			return m.transitionError(event, "application is already stopping")
		}
		snapshot.Terminal = true
		m.beginStop(&Disconnect{Initiator: InitiatorLocal, Reason: ReasonUserPause}, StatePaused)
	case EventStopCompleted:
		if snapshot.State != StateStopping || m.afterStop == "" {
			return m.transitionError(event, "no stop operation is active")
		}
		snapshot.State = m.afterStop
		m.afterStop = ""
		snapshot.ActionInProgress = false
		snapshot.Recovery = nil
		snapshot.Pairing = nil
		snapshot.Latency = 0
		snapshot.Docker = DockerStatus{State: ServiceStopped}
		snapshot.Sync = SyncStatus{State: ServiceStopped}
		m.connectionStarting = false
		m.connectionStartFrom = ""
	case EventTrustForgetStarted:
		if !allowed(*snapshot, CommandForget) {
			return m.transitionError(event, "trusted device cannot be forgotten in the current state")
		}
		if event.Peer == nil || snapshot.Peer == nil || strings.TrimSpace(event.Peer.ID) == "" || event.Peer.ID != snapshot.Peer.ID {
			return m.transitionError(event, "trusted device changed before cleanup started")
		}
		m.forgetting = true
		snapshot.ActionInProgress = true
	case EventTrustForgotten:
		if !m.forgetting {
			return m.transitionError(event, "trusted-device cleanup is not reserved")
		}
		snapshot.Peer = nil
		snapshot.TrustedPeers = 0
		snapshot.ActionInProgress = false
		m.forgetting = false
	case EventTrustForgetCancelled:
		if !m.forgetting {
			return m.transitionError(event, "trusted-device cleanup is not reserved")
		}
		snapshot.ActionInProgress = false
		m.forgetting = false
	case EventProblemDetected:
		if event.Problem == nil || event.Problem.Code == "" {
			return m.transitionError(event, "problem metadata is incomplete")
		}
		problem := *event.Problem
		snapshot.Problem = &problem
		if snapshot.State != StatePairing {
			if snapshot.State != StateNeedsAction {
				m.problemFrom = snapshot.State
			}
			snapshot.State = StateNeedsAction
		}
	case EventProblemCleared:
		if snapshot.Problem == nil || snapshot.State != StateNeedsAction && snapshot.State != StatePairing {
			return m.transitionError(event, "no problem is active")
		}
		snapshot.Problem = nil
		m.problemFrom = ""
		if snapshot.State == StateNeedsAction {
			snapshot.State = StatePaused
		}
	default:
		return m.transitionError(event, "event is unknown")
	}
	return nil
}

func (m *Machine) beginStop(disconnect *Disconnect, target State) {
	m.afterStop = target
	m.snapshot.State = StateStopping
	m.snapshot.ActionInProgress = true
	m.snapshot.Recovery = nil
	if disconnect != nil {
		value := *disconnect
		if value.At.IsZero() {
			value.At = m.now()
		}
		m.snapshot.LastDisconnect = &value
	}
}

func (m *Machine) idleState() State {
	if m.snapshot.Role == RoleWindowsHost {
		return StateHostWaiting
	}
	return StateClientReady
}

func (m *Machine) transitionError(event Event, cause string) error {
	return &TransitionError{State: m.snapshot.State, Event: event.Type, Cause: cause}
}

func (m *Machine) Subscribe() (<-chan Snapshot, func()) {
	updates := make(chan Snapshot, 1)
	if m == nil {
		close(updates)
		return updates, func() {}
	}
	m.mu.Lock()
	id := m.nextID
	m.nextID++
	m.subscribers[id] = updates
	updates <- m.snapshot.Clone()
	m.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			m.mu.Lock()
			if current, ok := m.subscribers[id]; ok {
				delete(m.subscribers, id)
				close(current)
			}
			m.mu.Unlock()
		})
	}
	return updates, cancel
}

func sixDigits(code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, character := range code {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
