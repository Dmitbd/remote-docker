// Package lifecycle owns the product-facing Remote Docker state machine.
// It deliberately contains no operating-system or UI dependencies.
package lifecycle

import "time"

const (
	ProblemTransportUpgradeRequired = "transport_upgrade_required"
	ProblemLocalSyncIdentityCorrupt = "local_sync_identity_corrupt"
	TransportUpgradeMessage         = "Сохранённое подключение использует старый транспорт. Забудьте устройство на Mac и Windows, затем выполните сопряжение ещё раз."
	TransportUpgradeAction          = "Забудьте старое доверие на обоих устройствах и один раз выполните безопасное сопряжение."
)

type Role string

const (
	RoleMacClient   Role = "mac_client"
	RoleWindowsHost Role = "windows_host"
)

type State string

const (
	StatePaused                     State = "paused"
	StateClientReady                State = "client_ready"
	StateSearching                  State = "searching"
	StateHostWaiting                State = "host_waiting"
	StatePairing                    State = "pairing"
	StatePairingCancellationPending State = "pairing_cancellation_pending"
	StateConnecting                 State = "connecting"
	StateConnected                  State = "connected"
	StateReconnecting               State = "reconnecting"
	StateStopping                   State = "stopping"
	StateNeedsAction                State = "needs_action"
)

type PairingStatus string

const (
	PairingPending             PairingStatus = "pending"
	PairingCancellationPending PairingStatus = "cancellation_pending"
	PairingApproved            PairingStatus = "approved"
	PairingRejected            PairingStatus = "rejected"
	PairingCompleted           PairingStatus = "completed"
	PairingExpired             PairingStatus = "expired"
)

type ServiceState string

const (
	ServiceStopped  ServiceState = "stopped"
	ServiceStarting ServiceState = "starting"
	ServiceReady    ServiceState = "ready"
	ServiceError    ServiceState = "error"
)

type Initiator string

const (
	InitiatorLocal  Initiator = "local"
	InitiatorPeer   Initiator = "peer"
	InitiatorSystem Initiator = "system"
)

type ConnectionReason string

const (
	ReasonUserDisconnect ConnectionReason = "user_disconnect"
	ReasonUserPause      ConnectionReason = "user_pause"
	ReasonPeerQuit       ConnectionReason = "peer_quit"
	ReasonNetworkTimeout ConnectionReason = "network_timeout"
	ReasonRuntimeFailure ConnectionReason = "runtime_failure"
)

type StopReason string

const (
	StopPause            StopReason = "pause"
	StopDisconnect       StopReason = "disconnect"
	StopCancelConnection StopReason = "cancel_connection"
	StopQuit             StopReason = "quit"
	StopFailure          StopReason = "failure"
)

type Peer struct {
	ID         string
	Name       string
	OS         string
	Version    string
	Address    string
	Generation string
}

type Pairing struct {
	SessionID string
	Peer      Peer
	Code      string
	Status    PairingStatus
	ExpiresAt time.Time
}

type DockerStatus struct {
	State   ServiceState
	Message string
}

type SyncStatus struct {
	State       ServiceState
	Pending     int64
	LastSuccess time.Time
}

type Recovery struct {
	Deadline time.Time
}

type Disconnect struct {
	Initiator Initiator
	Reason    ConnectionReason
	At        time.Time
}

type Problem struct {
	Code    string
	Device  Initiator
	Message string
	Action  string
}

type Snapshot struct {
	Revision         uint64
	Role             Role
	State            State
	LocalName        string
	Peer             *Peer
	TrustedPeers     int
	ConnectionLimit  int
	Pairing          *Pairing
	Docker           DockerStatus
	Sync             SyncStatus
	Latency          time.Duration
	Recovery         *Recovery
	LastDisconnect   *Disconnect
	ActionInProgress bool
	Problem          *Problem
	Terminal         bool
}

func (s Snapshot) Clone() Snapshot {
	cloned := s
	if s.Peer != nil {
		peer := *s.Peer
		cloned.Peer = &peer
	}
	if s.Pairing != nil {
		pairing := *s.Pairing
		cloned.Pairing = &pairing
	}
	if s.Recovery != nil {
		recovery := *s.Recovery
		cloned.Recovery = &recovery
	}
	if s.LastDisconnect != nil {
		disconnect := *s.LastDisconnect
		cloned.LastDisconnect = &disconnect
	}
	if s.Problem != nil {
		problem := *s.Problem
		cloned.Problem = &problem
	}
	return cloned
}
