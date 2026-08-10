// Package diagnostics runs the fixed, non-mutating health checks used by the
// local Doctor API. It accepts typed operations so platform runtimes can be
// composed without constructing shell commands from diagnostic input.
package diagnostics

import (
	"context"
)

// CheckName is a stable, safe name intended for public diagnostic output.
type CheckName string

const (
	CheckLANReachability CheckName = "lan_reachability"
	CheckTunnelIdentity  CheckName = "tunnel_identity"
	CheckTunnelSession   CheckName = "tunnel_session"
	CheckDockerChannel   CheckName = "docker_channel"
	CheckSyncChannel     CheckName = "sync_channel"
	CheckManagedWSL      CheckName = "managed_wsl"
)

var orderedCheckNames = []CheckName{
	CheckLANReachability,
	CheckTunnelIdentity,
	CheckTunnelSession,
	CheckDockerChannel,
	CheckSyncChannel,
	CheckManagedWSL,
}

// ErrCheckUnavailable is the public result for an operation not supplied by a
// platform runtime. It deliberately contains no machine-specific details.
var ErrCheckUnavailable = NewPublicError(ReasonCheckUnavailable)

// Check is one non-mutating diagnostic operation.
type Check interface {
	Check(context.Context) error
}

// CheckFunc adapts a function to Check.
type CheckFunc func(context.Context) error

func (f CheckFunc) Check(ctx context.Context) error {
	if f == nil {
		return ErrCheckUnavailable
	}
	return f(ctx)
}

// Operations supplies the fixed diagnostic operations. Each field has a
// single purpose and accepts no command strings or user-controlled arguments.
type Operations struct {
	LANReachability Check
	TunnelIdentity  Check
	TunnelSession   Check
	DockerChannel   Check
	SyncChannel     Check
	ManagedWSL      Check
}

func (o Operations) get(name CheckName) Check {
	switch name {
	case CheckLANReachability:
		return o.LANReachability
	case CheckTunnelIdentity:
		return o.TunnelIdentity
	case CheckTunnelSession:
		return o.TunnelSession
	case CheckDockerChannel:
		return o.DockerChannel
	case CheckSyncChannel:
		return o.SyncChannel
	case CheckManagedWSL:
		return o.ManagedWSL
	default:
		return nil
	}
}

func (o *Operations) set(name CheckName, check Check) {
	switch name {
	case CheckLANReachability:
		o.LANReachability = check
	case CheckTunnelIdentity:
		o.TunnelIdentity = check
	case CheckTunnelSession:
		o.TunnelSession = check
	case CheckDockerChannel:
		o.DockerChannel = check
	case CheckSyncChannel:
		o.SyncChannel = check
	case CheckManagedWSL:
		o.ManagedWSL = check
	}
}

// Result is one safe doctor result. Reason never returns an underlying error
// without redaction.
type Result struct {
	Name   CheckName `json:"name"`
	OK     bool      `json:"ok"`
	Reason string    `json:"reason,omitempty"`
}

// Runner executes every check in the contract order. Checks are intentionally
// not short-circuited: Doctor reports the current state of every subsystem.
type Runner struct {
	Operations Operations
}

// Check runs the complete fixed health-check sequence.
func (r Runner) Check(ctx context.Context) []Result {
	results := make([]Result, 0, len(orderedCheckNames))
	for _, name := range orderedCheckNames {
		operation := r.Operations.get(name)
		if operation == nil {
			results = append(results, Result{Name: name, Reason: string(ReasonCheckUnavailable)})
			continue
		}
		if err := operation.Check(ctx); err != nil {
			results = append(results, Result{Name: name, Reason: ReasonForError(err, ReasonCheckFailed)})
			continue
		}
		results = append(results, Result{Name: name, OK: true})
	}
	return results
}
