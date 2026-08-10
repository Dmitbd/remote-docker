package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/diagnostics"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/sshtransport"
	"github.com/Dmitbd/remote-docker/internal/windowsbridge"
)

func TestProductionRecoveryUsesOneOrchestrationPathAndFreshReadiness(t *testing.T) {
	observations := 0
	reconnects := 0
	userProcessRestarts := 0
	controller := &productionAgentController{diagnostics: newProductionDiagnosticsWithOptions(productionDiagnosticsOptions{
		Observe: func(context.Context) AgentStatus {
			observations++
			if observations == 1 {
				return AgentStatus{State: AgentConnecting, Message: "Authorization: Bearer observer-secret"}
			}
			return AgentStatus{State: AgentReady, Message: "internal ready detail"}
		},
		Reconnect: func(context.Context) error {
			reconnects++
			return nil
		},
		RestartUserProcess: func(context.Context) error {
			userProcessRestarts++
			return nil
		},
		Remote: staticRemoteDiagnostics{status: remoteDiagnosticStatus{
			WSLRunning: true, SystemdTarget: true, DiskAvailable: true,
		}},
		PortRelays: diagnostics.CheckFunc(func(context.Context) error { return nil }),
		Platform:   "darwin",
	})}

	raw, err := controller.Handle(context.Background(), localapi.MethodRecover, nil)
	if err != nil {
		t.Fatalf("Recover error = %v", err)
	}
	result, ok := raw.(localapi.RecoverResult)
	if !ok {
		t.Fatalf("Recover result type = %T, want localapi.RecoverResult", raw)
	}
	if reconnects != 1 || userProcessRestarts != 1 {
		t.Fatalf("reconnects=%d user-process restarts=%d, want one attempt per ladder step", reconnects, userProcessRestarts)
	}
	if observations != 2 {
		t.Fatalf("fresh observations = %d, want one after each action", observations)
	}
	if result.State != string(AgentReady) || result.Message != "connected" {
		t.Fatalf("result state = %#v, want fresh safe Ready state", result)
	}
	if len(result.Attempts) != 2 || result.Attempts[0].OK || !result.Attempts[1].OK {
		t.Fatalf("attempts = %#v, want readiness-gated second action", result.Attempts)
	}
	if result.Attempts[0].Reason != string(diagnostics.ReasonRecoveryNotConfirmed) {
		t.Fatalf("first attempt = %#v, want stable readiness reason", result.Attempts[0])
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "observer-secret") || strings.Contains(string(encoded), "internal ready detail") {
		t.Fatalf("public recovery leaked observer detail: %s", encoded)
	}
}

func TestProductionRecoveryReturnsPublicAttemptsAndNoRawFailureDetail(t *testing.T) {
	controller := &productionAgentController{diagnostics: newProductionDiagnosticsWithOptions(productionDiagnosticsOptions{
		Observe: func(context.Context) AgentStatus {
			return AgentStatus{State: AgentNeedsAction, Message: "ORDINARY_SETTING=quoted secret with spaces"}
		},
		Reconnect: func(context.Context) error {
			return errors.New("pairing_token=internal-pairing-secret")
		},
		Platform: "darwin",
	})}

	raw, err := controller.Handle(context.Background(), localapi.MethodRecover, nil)
	if err != nil {
		t.Fatalf("Recover error = %v", err)
	}
	result := raw.(localapi.RecoverResult)
	if result.State != string(AgentNeedsAction) || result.Message != "background agent needs attention" {
		t.Fatalf("result = %#v, want safe fresh needs-action state", result)
	}
	if len(result.Attempts) != 4 || result.Attempts[0].Reason != string(diagnostics.ReasonRecoveryOperationFailed) {
		t.Fatalf("attempts = %#v, want complete safe public ladder", result.Attempts)
	}
	encoded, _ := json.Marshal(result)
	for _, secret := range []string{"internal-pairing-secret", "quoted secret with spaces"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("public recovery retained secret %q: %s", secret, encoded)
		}
	}
}

func TestMacRepairStepsReconcileInfrastructureOnceBeforeReadiness(t *testing.T) {
	tests := []struct {
		name               string
		restartUser        bool
		wantStep           string
		wantUserRepairs    int
		wantSystemdRepairs int
	}{
		{name: "restart user process", restartUser: true, wantStep: "restart_user_process", wantUserRepairs: 1},
		{name: "restart systemd unit", wantStep: "restart_systemd_unit", wantSystemdRepairs: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready := false
			reconciles := 0
			userRepairs := 0
			userDockerReplays := 0
			remote := &repairingRemoteDiagnostics{status: remoteDiagnosticStatus{
				WSLRunning: true, SystemdTarget: true, DiskAvailable: true,
			}}
			options := productionDiagnosticsOptions{
				Observe: func(context.Context) AgentStatus {
					if ready {
						return AgentStatus{State: AgentReady}
					}
					return AgentStatus{State: AgentConnecting}
				},
				Reconnect: func(context.Context) error {
					return errors.New("initial reconnect did not repair the root cause")
				},
				Remote: remote,
				PortRelays: diagnostics.CheckFunc(func(context.Context) error {
					if !ready {
						return diagnostics.NewPublicError(diagnostics.ReasonPortRelaysNotReady)
					}
					return nil
				}),
				ReconcileAfterRepair: func(context.Context) error {
					reconciles++
					ready = true
					return nil
				},
				Platform: "darwin",
			}
			if tt.restartUser {
				options.RestartUserProcess = func(context.Context) error {
					userRepairs++
					return nil
				}
			}

			result, status, err := newProductionDiagnosticsWithOptions(options).Recover(context.Background())
			if err != nil {
				t.Fatalf("Recover() error = %v", err)
			}
			if string(result.Step) != tt.wantStep || status.State != AgentReady {
				t.Fatalf("result=%#v status=%#v, want repaired Ready via %s", result, status, tt.wantStep)
			}
			if reconciles != 1 || userRepairs != tt.wantUserRepairs || remote.restarts != tt.wantSystemdRepairs {
				t.Fatalf("reconciles=%d user repairs=%d systemd repairs=%d", reconciles, userRepairs, remote.restarts)
			}
			if userDockerReplays != 0 {
				t.Fatalf("repair replayed %d user Docker commands", userDockerReplays)
			}
		})
	}
}

func TestWindowsRecoveryRequiresFreshDockerAndSyncthingHealth(t *testing.T) {
	tests := []struct {
		name        string
		status      windowsbridge.ManagedWSLStatus
		wantReady   bool
		failedCheck string
	}{
		{
			name: "Docker unhealthy",
			status: windowsbridge.ManagedWSLStatus{
				Running: true, SystemdTarget: true, DiskAvailable: true, SyncthingService: true,
			},
			failedCheck: "docker_socket",
		},
		{
			name: "Syncthing unhealthy",
			status: windowsbridge.ManagedWSLStatus{
				Running: true, SystemdTarget: true, DockerSocket: true, DiskAvailable: true,
			},
			failedCheck: "syncthing",
		},
		{
			name: "all managed host checks healthy",
			status: windowsbridge.ManagedWSLStatus{
				Running: true, SystemdTarget: true, DockerSocket: true,
				DiskAvailable: true, SyncthingService: true,
			},
			wantReady: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			windows := &recordingWindowsDiagnostics{status: tt.status}
			diagnosticsRuntime := newProductionDiagnosticsWithOptions(productionDiagnosticsOptions{
				Observe:  func(context.Context) AgentStatus { return AgentStatus{State: AgentUnpaired} },
				Windows:  windows,
				Platform: "windows",
			})
			doctor := diagnosticsRuntime.Doctor(context.Background())
			if tt.failedCheck != "" {
				found := false
				for _, check := range doctor.Checks {
					if check.Name == tt.failedCheck {
						found = true
						if check.OK {
							t.Fatalf("Doctor check %s = %#v, want unhealthy remote result", tt.failedCheck, check)
						}
					}
				}
				if !found {
					t.Fatalf("Doctor checks = %#v, missing %s", doctor.Checks, tt.failedCheck)
				}
			}

			result, status, err := diagnosticsRuntime.Recover(context.Background())
			if tt.wantReady {
				if err != nil || status.State != AgentReady || result.Step != diagnostics.RecoveryStartWSLDistro {
					t.Fatalf("Recover() = (%#v, %#v, %v), want confirmed Windows self-heal", result, status, err)
				}
				return
			}
			if !errors.Is(err, diagnostics.ErrRecoveryFailed) || status.State == AgentReady || result.Step != "" {
				t.Fatalf("Recover() = (%#v, %#v, %v), must not synthesize Ready", result, status, err)
			}
		})
	}
}

func TestSSHRemoteDiagnosticsUsesOnlyExactTypedRPC(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		ActiveDevice:  "pc-1",
		Devices:       map[string]config.Device{"pc-1": {Address: "192.168.1.20"}},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	var commands []sshtransport.Command
	var methods []string
	client := sshRemoteDiagnostics{
		store: store, sshConfigPath: filepath.Join(root, "ssh_config"),
		run: func(_ context.Context, command sshtransport.Command) error {
			commands = append(commands, command)
			var request struct {
				Method string `json:"method"`
			}
			if err := json.NewDecoder(command.Stdin).Decode(&request); err != nil {
				return err
			}
			methods = append(methods, request.Method)
			response := map[string]any{"jsonrpc": "2.0", "id": 1}
			if request.Method == "diagnostics.observe" {
				response["result"] = remoteDiagnosticStatus{
					WSLRunning: true, SystemdTarget: true, DockerSocket: true,
					DiskAvailable: true, SyncthingService: true,
				}
			} else {
				response["result"] = map[string]bool{"restarted": true}
			}
			return json.NewEncoder(command.Stdout).Encode(response)
		},
	}

	status, err := client.Observe(context.Background())
	if err != nil || !status.WSLRunning || !status.SystemdTarget || !status.DockerSocket ||
		!status.DiskAvailable || !status.SyncthingService {
		t.Fatalf("Observe() = (%#v, %v)", status, err)
	}
	if err := client.RestartSystemdTarget(context.Background()); err != nil {
		t.Fatalf("RestartSystemdTarget() error = %v", err)
	}
	if !reflect.DeepEqual(methods, []string{"diagnostics.observe", "recovery.restart-systemd"}) {
		t.Fatalf("methods = %v, want exact RPC allowlist", methods)
	}
	wantArgs := []string{"-F", filepath.Join(root, "ssh_config"), "remote-docker-device-pc-1", "remote-docker-remote", "rpc"}
	for index, command := range commands {
		if command.Binary != "ssh" || !reflect.DeepEqual(command.Args, wantArgs) || command.Stderr != io.Discard {
			t.Fatalf("command[%d] = %#v, want exact managed SSH RPC", index, command)
		}
	}
}

func TestSSHRemoteDiagnosticsDoesNotPublishRemoteErrorOrOutput(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{SchemaVersion: config.CurrentSchemaVersion, ActiveDevice: "pc-1", Devices: map[string]config.Device{"pc-1": {}}}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	client := sshRemoteDiagnostics{store: store, run: func(_ context.Context, command sshtransport.Command) error {
		_, _ = bytes.NewBufferString("Authorization: Bearer remote-secret").WriteTo(command.Stdout)
		return errors.New("ORDINARY_SETTING=quoted secret with spaces")
	}}
	_, err := client.Observe(context.Background())
	if err == nil {
		t.Fatal("Observe() succeeded")
	}
	for _, secret := range []string{"remote-secret", "quoted secret with spaces"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("Observe() error retained secret %q: %v", secret, err)
		}
	}
}

func TestManagedSSHRestartClosesAndReplacesHealthyUserProcess(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		ActiveDevice:  "pc-1",
		Devices:       map[string]config.Device{"pc-1": {Address: "192.168.1.20", SSHPort: 49222}},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	secrets := newTestSSHSecretStore(t, "pc-1")
	var agents []*recordingManagedSSHAgent
	sshRuntime := &managedSSHRuntime{
		store: store, secrets: secrets,
		sshConfigPath:   filepath.Join(root, "ssh_config"),
		knownHostsPath:  filepath.Join(root, "known_hosts"),
		agentSocketPath: filepath.Join(root, "agent", "ssh-agent.sock"),
		controlDir:      filepath.Join(root, "control"),
		start: func(context.Context, string, []byte) (managedSSHAgent, error) {
			agent := &recordingManagedSSHAgent{}
			agents = append(agents, agent)
			return agent, nil
		},
		probe: func(context.Context, string) error { return nil },
	}
	if err := sshRuntime.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if err := sshRuntime.Restart(context.Background()); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	if len(agents) != 2 || agents[0].closes.Load() != 1 {
		t.Fatalf("agents=%d first closes=%d, want exact user-process replacement", len(agents), agents[0].closes.Load())
	}
}

func newTestSSHSecretStore(t *testing.T, deviceID string) *testSSHSecretStore {
	t.Helper()
	return &testSSHSecretStore{deviceID: deviceID, value: []byte("private-key")}
}

type testSSHSecretStore struct {
	deviceID string
	value    []byte
}

func (s *testSSHSecretStore) Put(string, string, []byte) error { return nil }
func (s *testSSHSecretStore) Get(deviceID, _ string) ([]byte, error) {
	if deviceID != s.deviceID {
		return nil, errors.New("not found")
	}
	return append([]byte(nil), s.value...), nil
}
func (s *testSSHSecretStore) Delete(string, string) error { return nil }

type staticRemoteDiagnostics struct {
	status     remoteDiagnosticStatus
	observeErr error
	restartErr error
}

func (s staticRemoteDiagnostics) Observe(context.Context) (remoteDiagnosticStatus, error) {
	return s.status, s.observeErr
}

func (s staticRemoteDiagnostics) RestartSystemdTarget(context.Context) error { return s.restartErr }

type repairingRemoteDiagnostics struct {
	status   remoteDiagnosticStatus
	restarts int
}

type recordingWindowsDiagnostics struct {
	status           windowsbridge.ManagedWSLStatus
	starts, restarts int
}

func (w *recordingWindowsDiagnostics) Observe(context.Context) (windowsbridge.ManagedWSLStatus, error) {
	return w.status, nil
}

func (w *recordingWindowsDiagnostics) StartDistro(context.Context) error {
	w.starts++
	return nil
}

func (w *recordingWindowsDiagnostics) RestartSystemdTarget(context.Context) error {
	w.restarts++
	return nil
}

func (r *repairingRemoteDiagnostics) Observe(context.Context) (remoteDiagnosticStatus, error) {
	return r.status, nil
}

func (r *repairingRemoteDiagnostics) RestartSystemdTarget(context.Context) error {
	r.restarts++
	return nil
}
