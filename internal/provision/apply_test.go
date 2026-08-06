package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestApplyRunsProvisioningInOrder(t *testing.T) {
	executor := &recordingApplyExecutor{}
	plan := Plan{Actions: []Action{{Kind: ActionImportDistro}}}

	result := Apply(context.Background(), plan, true, executor)

	if result.State != ApplyReady || result.Err != nil {
		t.Fatalf("Apply() = %#v, want ready", result)
	}
	want := []ApplyStep{
		StepCreateDataDirectory,
		StepVerifyRootfsChecksum,
		StepImportDistro,
		StepFirstBoot,
		StepHealthCheck,
		StepConfigurePrivateFirewall,
		StepRegisterAutostart,
	}
	if !reflect.DeepEqual(executor.steps, want) {
		t.Fatalf("steps = %#v, want %#v", executor.steps, want)
	}
}

func TestApplyStopsAfterEnablingWSLForReboot(t *testing.T) {
	executor := &recordingApplyExecutor{}
	plan := Plan{Actions: []Action{
		{Kind: ActionEnableWSL, RequiresReboot: true},
		{Kind: ActionImportDistro},
	}}

	result := Apply(context.Background(), plan, true, executor)

	if result.State != ApplyRebootRequired || result.Err != nil {
		t.Fatalf("Apply() = %#v, want reboot required", result)
	}
	if want := []ApplyStep{StepEnableWSL}; !reflect.DeepEqual(executor.steps, want) {
		t.Fatalf("steps = %#v, want %#v", executor.steps, want)
	}
}

func TestApplyDoesNotMutateWithoutConfirmationOrWithBlockers(t *testing.T) {
	tests := []struct {
		name      string
		plan      Plan
		confirmed bool
		wantErr   error
	}{
		{
			name:      "not confirmed",
			plan:      Plan{Actions: []Action{{Kind: ActionImportDistro}}},
			confirmed: false,
			wantErr:   ErrProvisioningNotConfirmed,
		},
		{
			name: "blocked",
			plan: Plan{Blockers: []Diagnostic{{
				Code: DiagnosticVirtualizationDisabled,
			}}},
			confirmed: true,
			wantErr:   ErrProvisioningBlocked,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := &recordingApplyExecutor{}
			result := Apply(context.Background(), tt.plan, tt.confirmed, executor)
			if result.State != ApplyFailedRecoverable || !errors.Is(result.Err, tt.wantErr) {
				t.Fatalf("Apply() = %#v, want recoverable %v", result, tt.wantErr)
			}
			if len(executor.steps) != 0 {
				t.Fatalf("mutating steps executed: %#v", executor.steps)
			}
		})
	}
}

func TestApplyReportsFailedStepAndStops(t *testing.T) {
	stepErr := errors.New("firewall unavailable")
	executor := &recordingApplyExecutor{
		failStep: StepConfigurePrivateFirewall,
		err:      stepErr,
	}

	result := Apply(context.Background(), Plan{}, true, executor)

	if result.State != ApplyFailedRecoverable || result.FailedStep != StepConfigurePrivateFirewall || !errors.Is(result.Err, stepErr) {
		t.Fatalf("Apply() = %#v", result)
	}
	want := []ApplyStep{StepHealthCheck, StepConfigurePrivateFirewall}
	if !reflect.DeepEqual(executor.steps, want) {
		t.Fatalf("steps = %#v, want %#v", executor.steps, want)
	}
}

func TestProvisionScriptKeepsConfirmationAndMutationOrder(t *testing.T) {
	script := readWindowsScript(t, "provision.ps1")
	ordered := []string{
		"if (-not $ConfirmProvisioning)",
		"Enable-WindowsOptionalFeature",
		"New-Item -ItemType Directory",
		"Get-FileHash",
		"'--import'",
		"$firstBoot",
		"Managed WSL health check",
		"New-NetFirewallRule",
		"Register-ScheduledTask",
	}
	last := -1
	for _, fragment := range ordered {
		position := strings.Index(script, fragment)
		if position < 0 {
			t.Fatalf("provision.ps1 is missing %q", fragment)
		}
		if position <= last {
			t.Fatalf("provision.ps1 fragment %q is out of order", fragment)
		}
		last = position
	}
	for _, fragment := range []string{"-Profile Private", "-RemoteAddress LocalSubnet"} {
		if !strings.Contains(script, fragment) {
			t.Fatalf("provision.ps1 is missing firewall restriction %q", fragment)
		}
	}
}

func TestUninstallPreservesDataUnlessSeparatelyConfirmed(t *testing.T) {
	script := readWindowsScript(t, "uninstall.ps1")
	preserve := strings.Index(script, "if (-not $DeleteData)")
	printName := strings.Index(script, "WSL distribution to delete: $managedDistroName")
	confirm := strings.Index(script, "$PSCmdlet.ShouldProcess")
	deleteDistro := strings.Index(script, "--unregister $managedDistroName")
	if preserve < 0 || printName <= preserve || confirm <= printName || deleteDistro <= confirm {
		t.Fatalf("uninstall.ps1 does not preserve and separately confirm data deletion")
	}
}

func readWindowsScript(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "packaging", "windows", "scripts", name)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(contents)
}

type recordingApplyExecutor struct {
	steps    []ApplyStep
	failStep ApplyStep
	err      error
}

func (e *recordingApplyExecutor) Run(_ context.Context, step ApplyStep) error {
	e.steps = append(e.steps, step)
	if step == e.failStep {
		return e.err
	}
	return nil
}
