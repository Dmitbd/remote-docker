package provision

const (
	minimumWindows11Build = 22000
	minimumFreeBytes      = uint64(40 << 30)
)

// ActionKind identifies a separately confirmable provisioning operation.
type ActionKind string

const (
	ActionEnableWSL    ActionKind = "enable_wsl"
	ActionUpdateWSL    ActionKind = "update_wsl"
	ActionImportDistro ActionKind = "import_managed_distro"
)

// Action describes intended work but never executes it.
type Action struct {
	Kind           ActionKind `json:"kind"`
	Description    string     `json:"description"`
	RequiresAdmin  bool       `json:"requires_admin"`
	RequiresReboot bool       `json:"requires_reboot"`
}

// DiagnosticCode identifies a blocking preflight condition.
type DiagnosticCode string

const (
	DiagnosticUnsupportedWindows     DiagnosticCode = "unsupported_windows"
	DiagnosticVirtualizationDisabled DiagnosticCode = "virtualization_disabled"
	DiagnosticDistroCollision        DiagnosticCode = "distro_name_collision"
	DiagnosticInsufficientDisk       DiagnosticCode = "insufficient_disk"
	DiagnosticFirewallUnavailable    DiagnosticCode = "firewall_capability_unavailable"
)

// Diagnostic explains why provisioning cannot proceed safely.
type Diagnostic struct {
	Code        DiagnosticCode `json:"code"`
	Message     string         `json:"message"`
	Remediation string         `json:"remediation"`
}

// Plan is an immutable description of proposed provisioning work.
type Plan struct {
	Actions        []Action     `json:"actions"`
	RequiresAdmin  bool         `json:"requires_admin"`
	RequiresReboot bool         `json:"requires_reboot"`
	Blockers       []Diagnostic `json:"blockers"`
}

// BuildPlan derives intended actions without changing Windows or WSL state.
func BuildPlan(probe ProbeResult) Plan {
	blockers := buildBlockers(probe)
	if len(blockers) > 0 {
		return Plan{Blockers: blockers}
	}

	actions := make([]Action, 0, 2)
	if !probe.WSLInstalled {
		actions = append(actions, Action{
			Kind:           ActionEnableWSL,
			Description:    "Enable Windows Subsystem for Linux and Virtual Machine Platform",
			RequiresAdmin:  true,
			RequiresReboot: true,
		})
	} else if !probe.WSL2Ready {
		actions = append(actions, Action{
			Kind:        ActionUpdateWSL,
			Description: "Install or update the WSL2 runtime",
		})
	}
	if !probe.Distro.Exists {
		actions = append(actions, Action{
			Kind:        ActionImportDistro,
			Description: "Import the isolated Remote Docker WSL distribution",
		})
	}

	plan := Plan{Actions: actions}
	for _, action := range actions {
		plan.RequiresAdmin = plan.RequiresAdmin || action.RequiresAdmin
		plan.RequiresReboot = plan.RequiresReboot || action.RequiresReboot
	}
	return plan
}

func buildBlockers(probe ProbeResult) []Diagnostic {
	var blockers []Diagnostic
	if probe.WindowsBuild < minimumWindows11Build {
		blockers = append(blockers, Diagnostic{
			Code:        DiagnosticUnsupportedWindows,
			Message:     "Windows 11 build 22000 or newer is required",
			Remediation: "Update Windows before provisioning Remote Docker",
		})
	}
	if !probe.VirtualizationEnabled {
		blockers = append(blockers, Diagnostic{
			Code:        DiagnosticVirtualizationDisabled,
			Message:     "Hardware virtualization is disabled or unavailable",
			Remediation: "Enable virtualization in firmware settings; the application will not change BIOS settings",
		})
	}
	if probe.Distro.Exists && !probe.Distro.MarkerMatches {
		blockers = append(blockers, Diagnostic{
			Code:        DiagnosticDistroCollision,
			Message:     "A WSL distribution already uses the managed distribution name",
			Remediation: "Rename or remove the unrelated distribution before continuing",
		})
	}
	if probe.FreeBytes < minimumFreeBytes {
		blockers = append(blockers, Diagnostic{
			Code:        DiagnosticInsufficientDisk,
			Message:     "At least 40 GiB of free disk space is required",
			Remediation: "Free disk space or choose another supported storage location",
		})
	}
	if !probe.FirewallCapability {
		blockers = append(blockers, Diagnostic{
			Code:        DiagnosticFirewallUnavailable,
			Message:     "Windows Firewall management capability is unavailable",
			Remediation: "Restore Windows Firewall management before exposing the local pairing service",
		})
	}
	return blockers
}
