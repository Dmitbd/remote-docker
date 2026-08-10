package localapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Dmitbd/remote-docker/internal/metrics"
)

const CurrentSchemaVersion = 4

type Method string

const (
	MethodStatus          Method = "Status"
	MethodShowWindow      Method = "ShowWindow"
	MethodEnable          Method = "Enable"
	MethodPause           Method = "Pause"
	MethodSearchStart     Method = "SearchStart"
	MethodSearchStop      Method = "SearchStop"
	MethodListDevices     Method = "ListDevices"
	MethodPairCandidates  Method = "PairCandidates"
	MethodPairStart       Method = "PairStart"
	MethodReplaceDevice   Method = "ReplaceDevice"
	MethodConnect         Method = "Connect"
	MethodPairStatus      Method = "PairStatus"
	MethodPairApprove     Method = "PairApprove"
	MethodPairReject      Method = "PairReject"
	MethodPairCancel      Method = "PairCancel"
	MethodDisconnect      Method = "Disconnect"
	MethodForgetDevice    Method = "ForgetDevice"
	MethodUnpair          Method = "Unpair"
	MethodWorkspaceAdd    Method = "WorkspaceAdd"
	MethodWorkspaceList   Method = "WorkspaceList"
	MethodWorkspaceRemove Method = "WorkspaceRemove"
	MethodSyncStatus      Method = "SyncStatus"
	MethodPrepareDocker   Method = "PrepareDocker"
	MethodDoctor          Method = "Doctor"
	MethodRecover         Method = "Recover"
	MethodShutdown        Method = "Shutdown"
	MethodResourceStatus  Method = "ResourceStatus"
)

func (m Method) valid() bool {
	switch m {
	case MethodStatus, MethodShowWindow, MethodEnable, MethodPause, MethodSearchStart, MethodSearchStop,
		MethodListDevices, MethodPairCandidates, MethodPairStart, MethodReplaceDevice, MethodConnect, MethodPairStatus,
		MethodPairApprove, MethodPairReject, MethodPairCancel,
		MethodDisconnect, MethodForgetDevice,
		MethodWorkspaceAdd, MethodWorkspaceList,
		MethodWorkspaceRemove, MethodSyncStatus, MethodPrepareDocker, MethodDoctor, MethodRecover,
		MethodShutdown, MethodResourceStatus:
		return true
	default:
		return false
	}
}

type ErrorCode string

const (
	ErrorInvalidRequest          ErrorCode = "invalid_request"
	ErrorSchemaMismatch          ErrorCode = "schema_mismatch"
	ErrorPeerForbidden           ErrorCode = "peer_forbidden"
	ErrorNeedsAction             ErrorCode = "needs_action"
	ErrorUnavailable             ErrorCode = "unavailable"
	ErrorRemoteRevokeUnavailable ErrorCode = "remote_revoke_unavailable"
	ErrorInternal                ErrorCode = "internal"
)

type PublicError struct {
	Code    ErrorCode
	Message string
}

func (e *PublicError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

type RemoteError struct {
	Code    ErrorCode
	Message string
}

func (e *RemoteError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("agent %s: %s", e.Code, e.Message)
}

type Handler interface {
	Handle(context.Context, Method, json.RawMessage) (any, error)
}

type HandlerFunc func(context.Context, Method, json.RawMessage) (any, error)

func (f HandlerFunc) Handle(ctx context.Context, method Method, params json.RawMessage) (any, error) {
	return f(ctx, method, params)
}

type StatusResult struct {
	Revision         uint64               `json:"revision,omitempty"`
	Role             string               `json:"role,omitempty"`
	State            string               `json:"state"`
	LocalName        string               `json:"local_name,omitempty"`
	Paired           bool                 `json:"paired"`
	Message          string               `json:"message,omitempty"`
	Peer             *LifecyclePeer       `json:"peer,omitempty"`
	TrustedPeers     int                  `json:"trusted_peers"`
	ConnectionLimit  int                  `json:"connection_limit,omitempty"`
	Pairing          *PairingStatusResult `json:"pairing,omitempty"`
	Docker           ServiceStatus        `json:"docker"`
	Sync             ServiceStatus        `json:"sync"`
	LatencyMS        int64                `json:"latency_ms,omitempty"`
	Recovery         *RecoveryStatus      `json:"recovery,omitempty"`
	LastDisconnect   *DisconnectStatus    `json:"last_disconnect,omitempty"`
	ActionInProgress bool                 `json:"action_in_progress,omitempty"`
	Problem          *ProblemStatus       `json:"problem,omitempty"`
	Terminal         bool                 `json:"terminal,omitempty"`
}

type LifecyclePeer struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	OS      string `json:"os,omitempty"`
	Version string `json:"version,omitempty"`
	Address string `json:"address,omitempty"`
}

type PairingStatusResult struct {
	SessionID string        `json:"session_id"`
	Peer      LifecyclePeer `json:"peer"`
	Code      string        `json:"code"`
	Status    string        `json:"status"`
	ExpiresAt string        `json:"expires_at"`
	Device    *Device       `json:"device,omitempty"`
}

type ServiceStatus struct {
	State       string `json:"state"`
	Message     string `json:"message,omitempty"`
	Pending     int64  `json:"pending_bytes,omitempty"`
	LastSuccess string `json:"last_success,omitempty"`
}

type RecoveryStatus struct {
	Deadline string `json:"deadline"`
}

type DisconnectStatus struct {
	Initiator string `json:"initiator"`
	Reason    string `json:"reason"`
	At        string `json:"at"`
}

type ProblemStatus struct {
	Code    string `json:"code"`
	Device  string `json:"device"`
	Message string `json:"message"`
	Action  string `json:"action"`
}

type LifecycleActionResult struct {
	Status StatusResult `json:"status"`
}

type ShutdownResult struct {
	Stopped bool `json:"stopped"`
}

type ResourceStatusParams struct {
	Active bool `json:"active"`
}

type ResourceStatusResult = metrics.Sample

type Device struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
}

type ListDevicesResult struct {
	Devices []Device `json:"devices"`
}

type PairingCandidate struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Unverified bool   `json:"unverified"`
	Trusted    bool   `json:"trusted"`
	Available  bool   `json:"available"`
}

type PairCandidatesResult struct {
	Candidates []PairingCandidate `json:"candidates"`
}

type PairStartParams struct {
	Device string `json:"device,omitempty"`
}

type ReplaceDeviceParams struct {
	OldDeviceID string `json:"old_device_id"`
	NewDevice   string `json:"new_device"`
	LocalOnly   bool   `json:"local_only,omitempty"`
}

type PairStartResult struct {
	SessionID string        `json:"session_id"`
	Code      string        `json:"code,omitempty"`
	Peer      LifecyclePeer `json:"peer"`
	ExpiresAt string        `json:"expires_at"`
}

type PairSessionParams struct {
	SessionID   string `json:"session_id"`
	ObserveOnly bool   `json:"observe_only,omitempty"`
}

type DisconnectParams struct {
	Reason string `json:"reason,omitempty"`
}

type ForgetDeviceParams struct {
	DeviceID  string `json:"device_id,omitempty"`
	LocalOnly bool   `json:"local_only,omitempty"`
}

type UnpairParams struct {
	DeviceID  string `json:"device_id,omitempty"`
	LocalOnly bool   `json:"local_only,omitempty"`
}

type Workspace struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type WorkspaceAddParams struct {
	Path string `json:"path"`
}

type WorkspaceRemoveParams struct {
	ID string `json:"id"`
}

type WorkspaceListResult struct {
	Workspaces []Workspace `json:"workspaces"`
}

type SyncFolderStatus struct {
	WorkspaceID string `json:"workspace_id"`
	State       string `json:"state"`
	Connected   bool   `json:"connected"`
}

type SyncStatusResult struct {
	Folders []SyncFolderStatus `json:"folders"`
}

// PrepareDockerParams contains only the invocation metadata required for the
// owner-scoped agent to enforce bind synchronization and port policy.
type PrepareDockerParams struct {
	BindSources      []string            `json:"bind_sources,omitempty"`
	StaticTCPPorts   []DockerPort        `json:"static_tcp_ports,omitempty"`
	Unsupported      []DockerUnsupported `json:"unsupported,omitempty"`
	WorkingDirectory string              `json:"working_directory"`
}

type DockerPort struct {
	HostIP        string `json:"host_ip,omitempty"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
}

type DockerUnsupported struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type PrepareDockerResult struct {
	Ready bool `json:"ready"`
}

type DoctorCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

type DoctorResult struct {
	Checks []DoctorCheck `json:"checks"`
}

type RecoverResult struct {
	State    string           `json:"state"`
	Message  string           `json:"message,omitempty"`
	Attempts []RecoverAttempt `json:"attempts,omitempty"`
}

type RecoverAttempt struct {
	Step   string `json:"step"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

type wireRequest struct {
	SchemaVersion int             `json:"schema_version"`
	ID            uint64          `json:"id"`
	Method        Method          `json:"method"`
	Params        json.RawMessage `json:"params,omitempty"`
}

type wireResponse struct {
	SchemaVersion int             `json:"schema_version"`
	ID            uint64          `json:"id"`
	Result        json.RawMessage `json:"result,omitempty"`
	Error         *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}
