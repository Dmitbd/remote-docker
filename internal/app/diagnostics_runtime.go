package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/diagnostics"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/sshtransport"
	"github.com/Dmitbd/remote-docker/internal/windowsbridge"
)

type remoteDiagnosticStatus struct {
	WSLRunning       bool `json:"wsl_running"`
	SystemdTarget    bool `json:"systemd_target"`
	DockerSocket     bool `json:"docker_socket"`
	DiskAvailable    bool `json:"disk_available"`
	SyncthingService bool `json:"syncthing_service"`
}

type remoteDiagnosticOperations interface {
	Observe(context.Context) (remoteDiagnosticStatus, error)
	RestartSystemdTarget(context.Context) error
}

type windowsDiagnosticOperations interface {
	Observe(context.Context) (windowsbridge.ManagedWSLStatus, error)
	StartDistro(context.Context) error
	RestartSystemdTarget(context.Context) error
}

type productionDiagnosticsOptions struct {
	Observe              func(context.Context) AgentStatus
	Reconnect            func(context.Context) error
	RestartUserProcess   func(context.Context) error
	ReconcileAfterRepair func(context.Context) error
	Remote               remoteDiagnosticOperations
	Windows              windowsDiagnosticOperations
	PortRelays           diagnostics.Check
	LANReachability      diagnostics.Check
	TunnelIdentity       diagnostics.Check
	TunnelSession        diagnostics.Check
	LocalRelays          diagnostics.Check
	DockerChannel        diagnostics.Check
	SyncChannel          diagnostics.Check
	ManagedWSL           diagnostics.Check
	Platform             string
}

// productionDiagnostics composes fixed platform operations. It accepts no
// user command, command arguments, shell text, or Docker invocation to replay.
type productionDiagnostics struct {
	options productionDiagnosticsOptions
}

func newProductionDiagnosticsWithOptions(options productionDiagnosticsOptions) productionDiagnostics {
	if options.Platform == "" {
		options.Platform = runtime.GOOS
	}
	return productionDiagnostics{options: options}
}

func (d productionDiagnostics) Doctor(ctx context.Context) localapi.DoctorResult {
	result, _ := d.observeHealth(ctx)
	return result
}

func (d productionDiagnostics) observeHealth(ctx context.Context) (localapi.DoctorResult, AgentStatus) {
	var status AgentStatus
	if d.options.Observe != nil {
		status = d.options.Observe(ctx)
	}
	localRelays := d.options.LocalRelays
	if localRelays == nil {
		localRelays = d.options.PortRelays
	}

	results := (diagnostics.Runner{Operations: diagnostics.Operations{
		LANReachability: d.options.LANReachability,
		TunnelIdentity:  d.options.TunnelIdentity,
		TunnelSession:   d.options.TunnelSession,
		LocalRelays:     localRelays,
		DockerChannel:   d.options.DockerChannel,
		SyncChannel:     d.options.SyncChannel,
		ManagedWSL:      d.options.ManagedWSL,
	}}).Check(ctx)
	checks := make([]localapi.DoctorCheck, 0, len(results))
	for _, result := range results {
		checks = append(checks, localapi.DoctorCheck{
			Name: string(result.Name), OK: result.OK, Message: result.Reason,
		})
	}
	return localapi.DoctorResult{Checks: checks}, status
}

func (d productionDiagnostics) Recover(ctx context.Context) (diagnostics.RecoveryResult, AgentStatus, error) {
	var latest AgentStatus
	readiness := diagnostics.ReadinessFunc(func(observeCtx context.Context) (bool, error) {
		health, observed := d.observeHealth(observeCtx)
		latest = safeAgentStatus(observed)
		if d.options.Platform == "windows" {
			ready := checksReady(health.Checks, "lan_reachability", "tunnel_identity", "tunnel_session", "local_relays", "docker_channel", "sync_channel", "managed_wsl")
			if ready {
				latest = AgentStatus{State: AgentReady, Paired: true, Message: "connected"}
			}
			return ready, nil
		}
		return latest.State == AgentReady && checksReady(health.Checks), nil
	})

	operations := diagnostics.RecoveryOperations{}
	repairThenReconcile := func(repair func(context.Context) error) diagnostics.RecoveryOperation {
		if repair == nil {
			return nil
		}
		return diagnostics.RecoveryFunc(func(recoveryCtx context.Context) error {
			if err := repair(recoveryCtx); err != nil {
				return err
			}
			if d.options.ReconcileAfterRepair == nil {
				return diagnostics.ErrRecoveryUnavailable
			}
			return d.options.ReconcileAfterRepair(recoveryCtx)
		})
	}
	if d.options.Reconnect != nil {
		operations.Reconnect = diagnostics.RecoveryFunc(d.options.Reconnect)
	}
	if d.options.RestartUserProcess != nil {
		operations.RestartUserProcess = repairThenReconcile(d.options.RestartUserProcess)
	}
	if d.options.Platform == "windows" && d.options.Windows != nil {
		operations.StartWSLDistro = diagnostics.RecoveryFunc(d.options.Windows.StartDistro)
		operations.RestartSystemdUnit = diagnostics.RecoveryFunc(d.options.Windows.RestartSystemdTarget)
	} else if d.options.Remote != nil {
		operations.RestartSystemdUnit = repairThenReconcile(d.options.Remote.RestartSystemdTarget)
	}

	result, err := (diagnostics.Recoverer{Operations: operations, Readiness: readiness}).Recover(ctx)
	if latest.State == "" {
		_, observed := d.observeHealth(ctx)
		latest = safeAgentStatus(observed)
	}
	return result, latest, err
}

func checksReady(checks []localapi.DoctorCheck, selected ...string) bool {
	selection := make(map[string]bool, len(selected))
	for _, name := range selected {
		selection[name] = true
	}
	found := 0
	for _, check := range checks {
		if len(selection) > 0 && !selection[check.Name] {
			continue
		}
		found++
		if !check.OK {
			return false
		}
	}
	if len(selection) > 0 {
		return found == len(selection)
	}
	return found == len(checks) && found > 0
}

func safeAgentStatus(status AgentStatus) AgentStatus {
	messages := map[AgentState]string{
		AgentUnpaired:       "pair a device to continue",
		AgentConnecting:     "establishing pinned SSH connection",
		AgentEngineStarting: "waiting for Docker Engine",
		AgentSyncing:        "waiting for Syncthing connection",
		AgentReady:          "connected",
		AgentDegraded:       "connection health check failed",
		AgentNeedsAction:    "background agent needs attention",
	}
	message, ok := messages[status.State]
	if !ok {
		return AgentStatus{State: AgentNeedsAction, Paired: status.Paired, Message: "background agent needs attention"}
	}
	return AgentStatus{State: status.State, Paired: status.Paired, Message: message}
}

type remoteDiagnosticsMethod string

const (
	remoteMethodObserve        remoteDiagnosticsMethod = "diagnostics.observe"
	remoteMethodRestartSystemd remoteDiagnosticsMethod = "recovery.restart-systemd"
)

// sshRemoteDiagnostics calls only the two fixed remote helper RPC methods over
// the pinned SSH alias. Neither the method nor process arguments are public.
type sshRemoteDiagnostics struct {
	store         config.Store
	sshConfigPath string
	sshBinary     string
	run           func(context.Context, sshtransport.Command) error
}

func (c sshRemoteDiagnostics) Observe(ctx context.Context) (remoteDiagnosticStatus, error) {
	var result remoteDiagnosticStatus
	if err := c.call(ctx, remoteMethodObserve, &result); err != nil {
		return remoteDiagnosticStatus{}, err
	}
	return result, nil
}

func (c sshRemoteDiagnostics) RestartSystemdTarget(ctx context.Context) error {
	var result struct {
		Restarted bool `json:"restarted"`
	}
	if err := c.call(ctx, remoteMethodRestartSystemd, &result); err != nil || !result.Restarted {
		return diagnostics.NewPublicError(diagnostics.ReasonSystemdTargetNotReady)
	}
	return nil
}

func (c sshRemoteDiagnostics) call(ctx context.Context, method remoteDiagnosticsMethod, destination any) error {
	if method != remoteMethodObserve && method != remoteMethodRestartSystemd {
		return errors.New("unsupported managed diagnostics RPC")
	}
	cfg, err := loadAgentConfig(c.store)
	if err != nil || cfg.ActiveDevice == "" {
		return errors.New("managed diagnostics device is unavailable")
	}
	if _, ok := cfg.Devices[cfg.ActiveDevice]; !ok {
		return errors.New("managed diagnostics device is unavailable")
	}
	request, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": string(method),
	})
	if err != nil {
		return errors.New("encode managed diagnostics RPC")
	}
	binary := c.sshBinary
	if binary == "" {
		binary = "ssh"
	}
	var output bytes.Buffer
	command := sshtransport.Command{
		Binary: binary,
		Args: []string{
			"-F", c.sshConfigPath, "remote-docker-device-" + cfg.ActiveDevice,
			"remote-docker-remote", "rpc",
		},
		Stdin: bytes.NewReader(append(request, '\n')), Stdout: &output, Stderr: io.Discard,
	}
	run := c.run
	if run == nil {
		run = runSSHCommand
	}
	if err := run(ctx, command); err != nil {
		return errors.New("managed diagnostics RPC failed")
	}
	var response struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(&output, 64<<10)).Decode(&response); err != nil ||
		response.JSONRPC != "2.0" || response.ID != 1 || len(response.Error) != 0 || len(response.Result) == 0 {
		return errors.New("managed diagnostics RPC was not acknowledged")
	}
	decoder := json.NewDecoder(bytes.NewReader(response.Result))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("managed diagnostics RPC returned invalid result")
	}
	return nil
}
