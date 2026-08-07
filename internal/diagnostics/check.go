// Package diagnostics runs the fixed, non-mutating health checks used by the
// local Doctor API. It accepts typed operations so platform runtimes can be
// composed without constructing shell commands from diagnostic input.
package diagnostics

import (
	"context"
	"errors"
)

// CheckName is a stable, safe name intended for public diagnostic output.
type CheckName string

const (
	CheckLANReachability CheckName = "lan_reachability"
	CheckSSHIdentity     CheckName = "ssh_identity"
	CheckWSLRunning      CheckName = "wsl_running"
	CheckSystemdTarget   CheckName = "systemd_target"
	CheckDockerSocket    CheckName = "docker_socket"
	CheckDisk            CheckName = "disk"
	CheckSyncthing       CheckName = "syncthing"
	CheckPortRelays      CheckName = "port_relays"
)

var orderedCheckNames = []CheckName{
	CheckLANReachability,
	CheckSSHIdentity,
	CheckWSLRunning,
	CheckSystemdTarget,
	CheckDockerSocket,
	CheckDisk,
	CheckSyncthing,
	CheckPortRelays,
}

// ErrCheckUnavailable is the public result for an operation not supplied by a
// platform runtime. It deliberately contains no machine-specific details.
var ErrCheckUnavailable = errors.New("diagnostic check is unavailable")

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
	SSHIdentity     Check
	WSLRunning      Check
	SystemdTarget   Check
	DockerSocket    Check
	Disk            Check
	Syncthing       Check
	PortRelays      Check
}

func (o Operations) get(name CheckName) Check {
	switch name {
	case CheckLANReachability:
		return o.LANReachability
	case CheckSSHIdentity:
		return o.SSHIdentity
	case CheckWSLRunning:
		return o.WSLRunning
	case CheckSystemdTarget:
		return o.SystemdTarget
	case CheckDockerSocket:
		return o.DockerSocket
	case CheckDisk:
		return o.Disk
	case CheckSyncthing:
		return o.Syncthing
	case CheckPortRelays:
		return o.PortRelays
	default:
		return nil
	}
}

func (o *Operations) set(name CheckName, check Check) {
	switch name {
	case CheckLANReachability:
		o.LANReachability = check
	case CheckSSHIdentity:
		o.SSHIdentity = check
	case CheckWSLRunning:
		o.WSLRunning = check
	case CheckSystemdTarget:
		o.SystemdTarget = check
	case CheckDockerSocket:
		o.DockerSocket = check
	case CheckDisk:
		o.Disk = check
	case CheckSyncthing:
		o.Syncthing = check
	case CheckPortRelays:
		o.PortRelays = check
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
			results = append(results, Result{Name: name, Reason: ErrCheckUnavailable.Error()})
			continue
		}
		if err := operation.Check(ctx); err != nil {
			results = append(results, Result{Name: name, Reason: RedactReason(err)})
			continue
		}
		results = append(results, Result{Name: name, OK: true})
	}
	return results
}
