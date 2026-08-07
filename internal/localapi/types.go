package localapi

import (
	"context"
	"encoding/json"
	"fmt"
)

const CurrentSchemaVersion = 2

type Method string

const (
	MethodStatus          Method = "Status"
	MethodListDevices     Method = "ListDevices"
	MethodPairCandidates  Method = "PairCandidates"
	MethodPairStart       Method = "PairStart"
	MethodPairConfirm     Method = "PairConfirm"
	MethodUnpair          Method = "Unpair"
	MethodWorkspaceAdd    Method = "WorkspaceAdd"
	MethodWorkspaceList   Method = "WorkspaceList"
	MethodWorkspaceRemove Method = "WorkspaceRemove"
	MethodSyncStatus      Method = "SyncStatus"
	MethodDoctor          Method = "Doctor"
	MethodRecover         Method = "Recover"
)

func (m Method) valid() bool {
	switch m {
	case MethodStatus, MethodListDevices, MethodPairCandidates, MethodPairStart, MethodPairConfirm,
		MethodUnpair, MethodWorkspaceAdd, MethodWorkspaceList,
		MethodWorkspaceRemove, MethodSyncStatus, MethodDoctor, MethodRecover:
		return true
	default:
		return false
	}
}

type ErrorCode string

const (
	ErrorInvalidRequest ErrorCode = "invalid_request"
	ErrorSchemaMismatch ErrorCode = "schema_mismatch"
	ErrorPeerForbidden  ErrorCode = "peer_forbidden"
	ErrorNeedsAction    ErrorCode = "needs_action"
	ErrorUnavailable    ErrorCode = "unavailable"
	ErrorInternal       ErrorCode = "internal"
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
	State   string `json:"state"`
	Paired  bool   `json:"paired"`
	Message string `json:"message,omitempty"`
}

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
}

type PairCandidatesResult struct {
	Candidates []PairingCandidate `json:"candidates"`
}

type PairStartParams struct {
	Device string `json:"device,omitempty"`
}

type PairStartResult struct {
	SessionID string `json:"session_id"`
	Code      string `json:"code,omitempty"`
}

type PairConfirmParams struct {
	SessionID string `json:"session_id"`
	Code      string `json:"code"`
}

type PairConfirmResult struct {
	Device Device `json:"device"`
}

type UnpairParams struct {
	DeviceID string `json:"device_id,omitempty"`
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
