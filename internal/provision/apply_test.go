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
		"Assert-ManagedDirectory -Path $ApplicationRoot",
		"New-Item -ItemType Directory",
		"Enable-WindowsOptionalFeature",
		"Get-FileHash",
		"'--import'",
		"$firstBoot = @(",
		"Managed WSL health check",
		"New-NetFirewallRule",
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
	for _, fragment := range []string{"-Program $desktopExecutable", "-Profile Private", "-RemoteAddress LocalSubnet"} {
		if !strings.Contains(script, fragment) {
			t.Fatalf("provision.ps1 is missing firewall restriction %q", fragment)
		}
	}
	for _, fragment := range []string{
		"Name = 'RemoteDocker.Managed.Tunnel.TCP'", "Protocol = 'TCP'",
		"Name = 'RemoteDocker.Managed.Discovery.UDP'", "Protocol = 'UDP'",
		"-LocalPort $PairingPort",
		"$legacyRule.Group -eq $firewallRuleGroup",
	} {
		if !strings.Contains(script, fragment) {
			t.Fatalf("provision.ps1 is missing secure tunnel firewall contract %q", fragment)
		}
	}
	for _, obsolete := range []string{"Port = $SshBridgePort", "Port = $SyncthingBridgePort"} {
		if strings.Contains(script, obsolete) {
			t.Fatalf("provision.ps1 still exposes obsolete LAN firewall port %q", obsolete)
		}
	}
	for _, fragment := range []string{"New-ScheduledTask", "Register-ScheduledTask", "RunLevel Highest"} {
		if strings.Contains(script, fragment) {
			t.Fatalf("provision.ps1 contains obsolete elevated startup registration %q", fragment)
		}
	}
}

func TestProvisionScriptReportsEarlyValidationFailuresToInstaller(t *testing.T) {
	script := readWindowsScript(t, "provision.ps1")

	tryBlock := strings.Index(script, "try {")
	confirmation := strings.Index(script, "if (-not $ConfirmProvisioning)")
	rootValidation := strings.Index(script, "Assert-ManagedDirectory -Path $ApplicationRoot")
	catchBlock := strings.LastIndex(script, "catch {")
	stderr := strings.Index(script, "[Console]::Error.WriteLine($reason)")
	exit := strings.Index(script, "exit 1")
	if tryBlock < 0 || confirmation <= tryBlock || rootValidation <= confirmation || catchBlock <= rootValidation || stderr <= catchBlock || exit <= stderr {
		t.Fatal("provision.ps1 does not report early validation failures through one installer-visible error boundary")
	}
	for _, fragment := range []string{"$logReady = $false", "$progressReady = $false"} {
		if !strings.Contains(script, fragment) {
			t.Fatalf("provision.ps1 is missing guarded failure state %q", fragment)
		}
	}
	if strings.Contains(script, "throw $reason") {
		t.Fatal("provision.ps1 rethrows the failure instead of returning one concise installer-visible error")
	}
}

func TestWindowsUpdaterStopsBeforeReplacingAnUnconfirmedDesktop(t *testing.T) {
	script := readWindowsPackagingFile(t, "install-agent.ps1")
	ordered := []string{
		"& $activePath --shutdown",
		"$shutdownExitCode = $LASTEXITCODE",
		"if ($shutdownExitCode -ne 0)",
		"Move-Item -LiteralPath $activePath -Destination $retiredPath",
		"        Wait-RemoteDockerProcessExit",
		"Move-Item -LiteralPath $stagedPath -Destination $activePath",
	}
	assertFragmentsInOrder(t, "install-agent.ps1", script, ordered)
}

func TestWindowsUpdaterPreservesRetiredBinaryUntilReplacementOrRestoreIsVerified(t *testing.T) {
	script := readWindowsPackagingFile(t, "install-agent.ps1")
	retire := strings.Index(script, "Move-Item -LiteralPath $activePath -Destination $retiredPath")
	unsafeCleanup := strings.Index(script, "$safeToCleanupStaging = $false")
	restore := strings.Index(script, "Move-Item -LiteralPath $retiredPath -Destination $activePath")
	restoreVerified := -1
	if restore >= 0 {
		if relative := strings.Index(script[restore:], "$safeToCleanupStaging = $true"); relative >= 0 {
			restoreVerified = restore + relative
		}
	}
	cleanupGuard := strings.LastIndex(script, "if ($safeToCleanupStaging -and (Test-Path -LiteralPath $stagingRoot))")
	cleanup := strings.LastIndex(script, "Remove-Item -LiteralPath $stagingRoot -Recurse -Force")
	if retire < 0 || unsafeCleanup <= retire || restore <= unsafeCleanup || restoreVerified <= restore || cleanupGuard <= restoreVerified || cleanup <= cleanupGuard {
		t.Fatal("install-agent.ps1 can delete the retired executable before replacement or restoration is verified")
	}
}

func TestWindowsInstallerStopsBeforeReplacingAnUnconfirmedDesktop(t *testing.T) {
	script := readWindowsPackagingFile(t, filepath.Join("installer", "RemoteDocker.nsi"))
	ordered := []string{
		"nsExec::ExecToStack /TIMEOUT=15000 '\"$INSTDIR\\RemoteDocker.exe\" --shutdown'",
		"${If} $0 != \"0\"",
		"Rename \"$INSTDIR\\RemoteDocker.exe\" \"$INSTDIR\\RemoteDocker.exe.upgrade-old\"",
		"-File \"$PLUGINSDIR\\wait-desktop-exit.ps1\"",
		"File /oname=RemoteDocker.exe \"${APP_SOURCE}\"",
	}
	assertFragmentsInOrder(t, "RemoteDocker.nsi", script, ordered)
}

func TestWindowsInstallerChecksRollbackBeforeClaimingSafeAbort(t *testing.T) {
	script := readWindowsPackagingFile(t, filepath.Join("installer", "RemoteDocker.nsi"))
	waitRestore := strings.Index(script, "Goto desktop_restore_binary")
	binaryFailed := strings.Index(script, "desktop_binary_failed:")
	binaryRestore := -1
	if binaryFailed >= 0 {
		if relative := strings.Index(script[binaryFailed:], "Goto desktop_restore_binary"); relative >= 0 {
			binaryRestore = binaryFailed + relative
		}
	}
	restoreLabel := strings.Index(script, "desktop_restore_binary:")
	clearErrors := -1
	if restoreLabel >= 0 {
		if relative := strings.Index(script[restoreLabel:], "ClearErrors"); relative >= 0 {
			clearErrors = restoreLabel + relative
		}
	}
	rename := strings.Index(script, "Rename \"$INSTDIR\\RemoteDocker.exe.upgrade-old\" \"$INSTDIR\\RemoteDocker.exe\"")
	restoreError := strings.Index(script, "IfErrors desktop_restore_failed")
	activeCheck := strings.Index(script, "IfFileExists \"$INSTDIR\\RemoteDocker.exe\" desktop_shutdown_failed desktop_restore_failed")
	restoreFailed := strings.Index(script, "desktop_restore_failed:")
	backupCheck := strings.Index(script, "IfFileExists \"$INSTDIR\\RemoteDocker.exe.upgrade-old\" desktop_restore_backup_preserved desktop_restore_backup_missing")
	if waitRestore < 0 || binaryFailed <= waitRestore || binaryRestore <= binaryFailed || restoreLabel <= binaryRestore || clearErrors <= restoreLabel || rename <= clearErrors || restoreError <= rename || activeCheck <= restoreError || restoreFailed <= activeCheck || backupCheck <= restoreFailed {
		t.Fatal("RemoteDocker.nsi can claim a safe abort without verifying the restored executable or preserving its backup")
	}
}

func TestProvisionFirstBootIsLFStableAndIdempotentAfterPartialImport(t *testing.T) {
	script := readWindowsScript(t, "provision.ps1")

	for _, fragment := range []string{
		"$distroExists = Test-ManagedDistro",
		"if (-not $distroExists)",
		"$firstBoot = @(",
		") -join \"`n\"",
		"managed.json.tmp",
		"mv -f /etc/remote-docker/managed.json.tmp /etc/remote-docker/managed.json",
		"/etc/remote-docker/managed.json",
	} {
		if !strings.Contains(script, fragment) {
			t.Fatalf("provision.ps1 is missing recoverable LF-only first boot fragment %q", fragment)
		}
	}
	if strings.Contains(script, "$firstBoot = @'") {
		t.Fatal("provision.ps1 first boot uses a source-newline-dependent here-string")
	}

	firstBoot := strings.Index(script, "$firstBoot = @(")
	invoke := strings.Index(script, "-Description 'Managed WSL first boot'")
	if firstBoot < 0 || invoke <= firstBoot {
		t.Fatal("provision.ps1 does not invoke the recoverable first boot script")
	}
}

func TestProvisionRecreatesManagedMetadataWithoutDependingOnItsPresence(t *testing.T) {
	script := readWindowsScript(t, "provision.ps1")

	for _, fragment := range []string{
		"$distroExists = Test-ManagedDistro",
		"$firstBoot = @(",
		"install -d -m 0755 /etc/remote-docker",
		"managed.json.tmp",
		"-Description 'Managed WSL first boot'",
	} {
		if !strings.Contains(script, fragment) {
			t.Fatalf("provision.ps1 is missing idempotent metadata preparation fragment %q", fragment)
		}
	}
	if strings.Contains(script, "managedMetadataProbe") || strings.Contains(script, "firstBootRequired") {
		t.Fatal("provision.ps1 must not require optional managed metadata before idempotent first boot")
	}
}

func TestUninstallPreservesDataUnlessSeparatelyConfirmed(t *testing.T) {
	script := readWindowsScript(t, "uninstall.ps1")
	preserve := strings.Index(script, "if (-not $DeleteData)")
	exactPhrase := strings.Index(script, "$DataRemovalConfirmation -ne 'DELETE-REMOTE-DOCKER-DATA'")
	ownership := strings.Index(script, "Managed data ownership marker is missing or invalid")
	confirm := strings.Index(script, "$PSCmdlet.ShouldProcess")
	deleteDistro := strings.Index(script, "Invoke-Wsl -ArgumentList @('--unregister', $managedDistroName)")
	deleteTree := strings.Index(script, "Remove-Item -LiteralPath $distroRoot -Recurse -Force")
	if preserve < 0 || exactPhrase <= preserve || ownership <= exactPhrase || confirm <= ownership || deleteDistro <= confirm || deleteTree <= confirm {
		t.Fatalf("uninstall.ps1 does not preserve and separately confirm data deletion")
	}
	if strings.Contains(script, "Remove-Item -LiteralPath $ApplicationRoot -Recurse") {
		t.Fatal("uninstall.ps1 must not recursively remove the selected application root")
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

func readWindowsPackagingFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "packaging", "windows", name)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(contents)
}

func assertFragmentsInOrder(t *testing.T, name, contents string, fragments []string) {
	t.Helper()
	last := -1
	for _, fragment := range fragments {
		position := strings.Index(contents, fragment)
		if position < 0 {
			t.Fatalf("%s is missing %q", name, fragment)
		}
		if position <= last {
			t.Fatalf("%s fragment %q is out of order", name, fragment)
		}
		last = position
	}
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
