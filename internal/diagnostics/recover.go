package diagnostics

import (
	"context"
	"errors"
)

// RecoveryStep is a stable, safe name for one allowed recovery action.
type RecoveryStep string

const (
	RecoveryReconnect          RecoveryStep = "reconnect"
	RecoveryRestartUserProcess RecoveryStep = "restart_user_process"
	RecoveryStartWSLDistro     RecoveryStep = "start_wsl_distro"
	RecoveryRestartSystemdUnit RecoveryStep = "restart_systemd_unit"
)

var orderedRecoverySteps = []RecoveryStep{
	RecoveryReconnect,
	RecoveryRestartUserProcess,
	RecoveryStartWSLDistro,
	RecoveryRestartSystemdUnit,
}

var (
	// ErrRecoveryUnavailable is used for a missing typed recovery operation.
	ErrRecoveryUnavailable = errors.New("recovery operation is unavailable")
	// ErrRecoveryFailed is deliberately generic so internal or secret causes
	// cannot cross the local API boundary.
	ErrRecoveryFailed = errors.New("remote environment recovery did not complete")
)

// RecoveryOperation is one allowlisted, typed action. It deliberately has no
// command, argument, or script field; destructive Docker/WSL actions cannot be
// represented by this package.
type RecoveryOperation interface {
	Recover(context.Context) error
}

// RecoveryFunc adapts a function to RecoveryOperation.
type RecoveryFunc func(context.Context) error

func (f RecoveryFunc) Recover(ctx context.Context) error {
	if f == nil {
		return ErrRecoveryUnavailable
	}
	return f(ctx)
}

// RecoveryOperations supplies exactly the non-destructive recovery ladder.
type RecoveryOperations struct {
	Reconnect          RecoveryOperation
	RestartUserProcess RecoveryOperation
	StartWSLDistro     RecoveryOperation
	RestartSystemdUnit RecoveryOperation
}

func (o RecoveryOperations) get(step RecoveryStep) RecoveryOperation {
	switch step {
	case RecoveryReconnect:
		return o.Reconnect
	case RecoveryRestartUserProcess:
		return o.RestartUserProcess
	case RecoveryStartWSLDistro:
		return o.StartWSLDistro
	case RecoveryRestartSystemdUnit:
		return o.RestartSystemdUnit
	default:
		return nil
	}
}

// Attempt records a safe outcome from one allowed operation.
type Attempt struct {
	Step   RecoveryStep `json:"step"`
	OK     bool         `json:"ok"`
	Reason string       `json:"reason,omitempty"`
}

// RecoveryResult is safe to return through a public local API.
type RecoveryResult struct {
	Step     RecoveryStep `json:"step,omitempty"`
	Attempts []Attempt    `json:"attempts"`
}

// Recoverer performs the fixed recovery ladder and stops after the first
// successful action. No Docker cleanup, volume removal, or WSL deregistration
// action exists in its type system.
type Recoverer struct {
	Operations RecoveryOperations
}

// Recover performs recovery in the only supported order.
func (r Recoverer) Recover(ctx context.Context) (RecoveryResult, error) {
	result := RecoveryResult{Attempts: make([]Attempt, 0, len(orderedRecoverySteps))}
	for _, step := range orderedRecoverySteps {
		operation := r.Operations.get(step)
		if operation == nil {
			result.Attempts = append(result.Attempts, Attempt{Step: step, Reason: ErrRecoveryUnavailable.Error()})
			continue
		}
		if err := operation.Recover(ctx); err != nil {
			result.Attempts = append(result.Attempts, Attempt{Step: step, Reason: RedactReason(err)})
			continue
		}
		result.Step = step
		result.Attempts = append(result.Attempts, Attempt{Step: step, OK: true})
		return result, nil
	}
	return result, ErrRecoveryFailed
}
