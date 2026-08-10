package provision

import (
	"reflect"
	"testing"
)

func TestBuildPlanMatrix(t *testing.T) {
	ready := ProbeResult{
		WindowsBuild:          22631,
		VirtualizationEnabled: true,
		WSLInstalled:          true,
		WSL2Ready:             true,
		FreeBytes:             80 << 30,
		FirewallCapability:    true,
	}

	tests := []struct {
		name         string
		probe        ProbeResult
		wantActions  []ActionKind
		wantAdmin    bool
		wantReboot   bool
		wantBlockers []DiagnosticCode
	}{
		{
			name:        "Windows 11 and WSL2 ready",
			probe:       ready,
			wantActions: []ActionKind{ActionImportDistro},
		},
		{
			name: "WSL missing",
			probe: mutateProbe(ready, func(probe *ProbeResult) {
				probe.WSLInstalled = false
				probe.WSL2Ready = false
			}),
			wantActions: []ActionKind{ActionEnableWSL, ActionImportDistro},
			wantAdmin:   true,
			wantReboot:  true,
		},
		{
			name: "virtualization disabled",
			probe: mutateProbe(ready, func(probe *ProbeResult) {
				probe.VirtualizationEnabled = false
			}),
			wantBlockers: []DiagnosticCode{DiagnosticVirtualizationDisabled},
		},
		{
			name: "matching managed distro is reused",
			probe: mutateProbe(ready, func(probe *ProbeResult) {
				probe.Distro = DistroProbe{Exists: true, MarkerMatches: true}
			}),
		},
		{
			name: "distro name collision",
			probe: mutateProbe(ready, func(probe *ProbeResult) {
				probe.Distro = DistroProbe{Exists: true, MarkerMatches: false}
			}),
			wantBlockers: []DiagnosticCode{DiagnosticDistroCollision},
		},
		{
			name: "less than 40 GiB free",
			probe: mutateProbe(ready, func(probe *ProbeResult) {
				probe.FreeBytes = (40 << 30) - 1
			}),
			wantBlockers: []DiagnosticCode{DiagnosticInsufficientDisk},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probeBefore := tt.probe
			plan := BuildPlan(tt.probe)

			if got := actionKinds(plan.Actions); !reflect.DeepEqual(got, tt.wantActions) {
				t.Fatalf("Actions = %#v, want %#v", got, tt.wantActions)
			}
			if plan.RequiresAdmin != tt.wantAdmin {
				t.Fatalf("RequiresAdmin = %v, want %v", plan.RequiresAdmin, tt.wantAdmin)
			}
			if plan.RequiresReboot != tt.wantReboot {
				t.Fatalf("RequiresReboot = %v, want %v", plan.RequiresReboot, tt.wantReboot)
			}
			if got := diagnosticCodes(plan.Blockers); !reflect.DeepEqual(got, tt.wantBlockers) {
				t.Fatalf("Blockers = %#v, want %#v", got, tt.wantBlockers)
			}
			if !reflect.DeepEqual(tt.probe, probeBefore) {
				t.Fatalf("BuildPlan mutated ProbeResult: before=%#v after=%#v", probeBefore, tt.probe)
			}
		})
	}
}

func TestBuildPlanBlocksUnsupportedWindowsAndFirewall(t *testing.T) {
	plan := BuildPlan(ProbeResult{
		WindowsBuild:          19045,
		VirtualizationEnabled: true,
		WSLInstalled:          true,
		WSL2Ready:             true,
		FreeBytes:             80 << 30,
		FirewallCapability:    false,
	})
	want := []DiagnosticCode{DiagnosticUnsupportedWindows, DiagnosticFirewallUnavailable}
	if got := diagnosticCodes(plan.Blockers); !reflect.DeepEqual(got, want) {
		t.Fatalf("Blockers = %#v, want %#v", got, want)
	}
	if len(plan.Actions) != 0 || plan.RequiresAdmin || plan.RequiresReboot {
		t.Fatalf("blocked plan contains mutation intent: %#v", plan)
	}
}

func actionKinds(actions []Action) []ActionKind {
	if len(actions) == 0 {
		return nil
	}
	result := make([]ActionKind, 0, len(actions))
	for _, action := range actions {
		result = append(result, action.Kind)
	}
	return result
}

func diagnosticCodes(diagnostics []Diagnostic) []DiagnosticCode {
	if len(diagnostics) == 0 {
		return nil
	}
	result := make([]DiagnosticCode, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		result = append(result, diagnostic.Code)
	}
	return result
}

func mutateProbe(base ProbeResult, mutate func(*ProbeResult)) ProbeResult {
	mutate(&base)
	return base
}
