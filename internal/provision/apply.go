package provision

import (
	"context"
	"errors"
	"fmt"
)

// ApplyState is the typed outcome of a provisioning attempt.
type ApplyState string

const (
	ApplyReady             ApplyState = "ready"
	ApplyRebootRequired    ApplyState = "reboot_required"
	ApplyFailedRecoverable ApplyState = "failed_recoverable"
)

var (
	ErrProvisioningNotConfirmed = errors.New("provisioning was not explicitly confirmed")
	ErrProvisioningBlocked      = errors.New("provisioning plan contains blockers")
	ErrApplyExecutorUnavailable = errors.New("provisioning executor is unavailable")
	ErrUnsupportedAction        = errors.New("unsupported provisioning action")
)

// ApplyStep names one ordered operation performed by the platform executor.
type ApplyStep string

const (
	StepEnableWSL                ApplyStep = "enable_wsl"
	StepUpdateWSL                ApplyStep = "update_wsl"
	StepCreateDataDirectory      ApplyStep = "create_data_directory"
	StepVerifyRootfsChecksum     ApplyStep = "verify_rootfs_checksum"
	StepImportDistro             ApplyStep = "import_distro"
	StepFirstBoot                ApplyStep = "first_boot"
	StepHealthCheck              ApplyStep = "health_check"
	StepConfigurePrivateFirewall ApplyStep = "configure_private_firewall"
	StepRegisterAutostart        ApplyStep = "register_autostart"
)

// ApplyExecutor maps a reviewed step to the platform-specific implementation.
type ApplyExecutor interface {
	Run(context.Context, ApplyStep) error
}

// ApplyResult records a stable state and, for failures, the exact failed step.
type ApplyResult struct {
	State      ApplyState
	FailedStep ApplyStep
	Err        error
}

// Apply executes an immutable plan only after explicit confirmation.
func Apply(ctx context.Context, plan Plan, confirmed bool, executor ApplyExecutor) ApplyResult {
	if len(plan.Blockers) > 0 {
		return failedApply("", fmt.Errorf("%w: %d diagnostic(s)", ErrProvisioningBlocked, len(plan.Blockers)))
	}
	if !confirmed {
		return failedApply("", ErrProvisioningNotConfirmed)
	}
	if executor == nil {
		return failedApply("", ErrApplyExecutorUnavailable)
	}

	needsImport := false
	for _, action := range plan.Actions {
		switch action.Kind {
		case ActionEnableWSL:
			if result, failed := runApplyStep(ctx, executor, StepEnableWSL); failed {
				return result
			}
			return ApplyResult{State: ApplyRebootRequired}
		case ActionUpdateWSL:
			if result, failed := runApplyStep(ctx, executor, StepUpdateWSL); failed {
				return result
			}
		case ActionImportDistro:
			needsImport = true
		default:
			return failedApply("", fmt.Errorf("%w: %q", ErrUnsupportedAction, action.Kind))
		}
	}

	if needsImport {
		for _, step := range []ApplyStep{
			StepCreateDataDirectory,
			StepVerifyRootfsChecksum,
			StepImportDistro,
			StepFirstBoot,
		} {
			if result, failed := runApplyStep(ctx, executor, step); failed {
				return result
			}
		}
	}

	for _, step := range []ApplyStep{
		StepHealthCheck,
		StepConfigurePrivateFirewall,
		StepRegisterAutostart,
	} {
		if result, failed := runApplyStep(ctx, executor, step); failed {
			return result
		}
	}

	return ApplyResult{State: ApplyReady}
}

func runApplyStep(ctx context.Context, executor ApplyExecutor, step ApplyStep) (ApplyResult, bool) {
	if err := executor.Run(ctx, step); err != nil {
		return failedApply(step, fmt.Errorf("provisioning step %s: %w", step, err)), true
	}
	return ApplyResult{}, false
}

func failedApply(step ApplyStep, err error) ApplyResult {
	return ApplyResult{State: ApplyFailedRecoverable, FailedStep: step, Err: err}
}
