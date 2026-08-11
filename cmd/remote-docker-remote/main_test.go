package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/metrics"
	"golang.org/x/crypto/ssh"
)

func TestMetricsResultMapPreservesAvailabilityInsteadOfInventingZero(t *testing.T) {
	result := metricsResultMap(metrics.RemoteSample{
		DockerContainers: metrics.Unavailable[int]("Docker is stopped"),
	})
	containers, ok := result["docker_containers"].(map[string]any)
	if !ok || containers["available"] != false || containers["reason"] != "Docker is stopped" {
		t.Fatalf("metrics result = %#v", result)
	}
}

func TestRPCHealthAndMethodAllowlist(t *testing.T) {
	input := strings.NewReader(
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"health\"}\n" +
			"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"shell.exec\"}\n",
	)
	var output bytes.Buffer
	var stderr bytes.Buffer

	if code := runRPC(input, &output, &stderr); code != 0 {
		t.Fatalf("runRPC() code = %d, stderr = %s", code, &stderr)
	}
	decoder := json.NewDecoder(&output)
	var health response
	if err := decoder.Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if health.Error != nil || health.Result["status"] != "ok" {
		t.Fatalf("health response = %#v", health)
	}
	var rejected response
	if err := decoder.Decode(&rejected); err != nil {
		t.Fatalf("decode rejected response: %v", err)
	}
	if rejected.Error == nil || rejected.Error.Code != -32601 {
		t.Fatalf("rejected response = %#v", rejected)
	}
}

func TestPairingInstallAndRevokeOwnOnlyManagedAuthorizedKey(t *testing.T) {
	root := t.TempDir()
	hostKey := authorizedKey(t)
	macKey := authorizedKey(t)
	hostKeyPath := filepath.Join(root, "ssh_host_ed25519_key.pub")
	authorizedKeysPath := filepath.Join(root, "authorized_keys")
	if err := os.WriteFile(hostKeyPath, []byte(hostKey+"\n"), 0o600); err != nil {
		t.Fatalf("write host public key: %v", err)
	}
	runtime := pairingRuntime{
		HostPublicKeyPath:  hostKeyPath,
		AuthorizedKeysPath: authorizedKeysPath,
		SyncthingDeviceID:  func(context.Context) (string, error) { return "WINDOWS-SYNCTHING-ID", nil },
	}
	var output bytes.Buffer
	if code := runPairingInstall(context.Background(), runtime, []string{"--device", "mac-device"}, strings.NewReader(macKey+"\n"), &output, &bytes.Buffer{}); code != 0 {
		t.Fatalf("runPairingInstall() code = %d", code)
	}
	var installed pairingDeviceInfo
	if err := json.Unmarshal(output.Bytes(), &installed); err != nil {
		t.Fatalf("decode install response: %v", err)
	}
	if installed.SSHHostPublicKey != hostKey || installed.SyncthingDeviceID != "WINDOWS-SYNCTHING-ID" {
		t.Fatalf("installed = %#v", installed)
	}
	contents, err := os.ReadFile(authorizedKeysPath)
	if err != nil {
		t.Fatalf("read authorized keys: %v", err)
	}
	if !strings.Contains(string(contents), macKey) || !strings.Contains(string(contents), "remote-docker-device=mac-device") {
		t.Fatalf("authorized keys = %q", contents)
	}
	otherKey := authorizedKey(t)
	if code := runPairingInstall(context.Background(), runtime, []string{"--device", "other-device"}, strings.NewReader(otherKey+"\n"), &bytes.Buffer{}, &bytes.Buffer{}); code == 0 {
		t.Fatal("pairing install replaced an existing trusted device")
	}
	if unchanged, readErr := os.ReadFile(authorizedKeysPath); readErr != nil || !bytes.Equal(unchanged, contents) {
		t.Fatalf("existing authorization changed: %q error=%v", unchanged, readErr)
	}

	if code := runPairingRevoke(runtime, []string{"--device", "other-device"}, &bytes.Buffer{}); code == 0 {
		t.Fatal("revoke for another device succeeded")
	}
	if after, _ := os.ReadFile(authorizedKeysPath); !bytes.Equal(after, contents) {
		t.Fatalf("another device changed authorized keys: %q", after)
	}
	if code := runPairingRevoke(runtime, []string{"--device", "mac-device"}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("runPairingRevoke() code = %d", code)
	}
	if after, err := os.ReadFile(authorizedKeysPath); err != nil || len(after) != 0 {
		t.Fatalf("authorized keys after revoke = %q, %v", after, err)
	}
}

func TestPairingRevokeRejectsDeviceIDPrefixCollision(t *testing.T) {
	root := t.TempDir()
	authorizedKeysPath := filepath.Join(root, "authorized_keys")
	contents := []byte(testAuthorizedLine(t, "mac-device-long"))
	if err := os.WriteFile(authorizedKeysPath, contents, 0o600); err != nil {
		t.Fatalf("seed authorized keys: %v", err)
	}
	runtime := pairingRuntime{AuthorizedKeysPath: authorizedKeysPath}

	if code := runPairingRevoke(runtime, []string{"--device", "mac-device"}, &bytes.Buffer{}); code == 0 {
		t.Fatal("revoke accepted a device ID that is only a prefix of the managed owner")
	}
	after, err := os.ReadFile(authorizedKeysPath)
	if err != nil {
		t.Fatalf("read authorized keys after rejected revoke: %v", err)
	}
	if !bytes.Equal(after, contents) {
		t.Fatalf("prefix collision changed authorized keys: %q", after)
	}
}

func TestRPCPairingRevokeUsesManagedAuthorizationRuntime(t *testing.T) {
	root := t.TempDir()
	runtime := pairingRuntime{
		AuthorizedKeysPath: filepath.Join(root, "authorized_keys"),
	}
	if err := os.WriteFile(runtime.AuthorizedKeysPath, []byte(testAuthorizedLine(t, "mac-device")), 0o600); err != nil {
		t.Fatalf("seed authorized keys: %v", err)
	}
	input := strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"pairing.revoke","params":{"device_id":"mac-device"}}` + "\n")
	var output bytes.Buffer
	syncOperations := &recordingRemoteSync{}
	if code := runRPCWithAllOperations(input, &output, &bytes.Buffer{}, runtime, nil, syncOperations); code != 0 {
		t.Fatalf("runRPCWithAllOperations() code = %d", code)
	}
	var result response
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Error != nil || result.Result["revoked"] != true {
		t.Fatalf("response = %#v", result)
	}
	if contents, _ := os.ReadFile(runtime.AuthorizedKeysPath); len(contents) != 0 {
		t.Fatalf("authorized keys after RPC revoke = %q", contents)
	}
	if syncOperations.revoked != "mac-device" {
		t.Fatalf("revoked Syncthing device = %q, want mac-device", syncOperations.revoked)
	}
}

func TestPairingRevokeCommandRemovesSyncthingAndSSHTrust(t *testing.T) {
	root := t.TempDir()
	runtime := pairingRuntime{AuthorizedKeysPath: filepath.Join(root, "authorized_keys")}
	if err := os.WriteFile(runtime.AuthorizedKeysPath, []byte(testAuthorizedLine(t, "mac-device")), 0o600); err != nil {
		t.Fatalf("seed authorized keys: %v", err)
	}
	syncOperations := &recordingRemoteSync{}
	if code := runPairingRevokeCommand(
		context.Background(), runtime, syncOperations, []string{"--device", "mac-device"}, &bytes.Buffer{},
	); code != 0 {
		t.Fatalf("runPairingRevokeCommand() code = %d", code)
	}
	if syncOperations.revoked != "mac-device" {
		t.Fatalf("revoked Syncthing device = %q, want mac-device", syncOperations.revoked)
	}
	if contents, _ := os.ReadFile(runtime.AuthorizedKeysPath); len(contents) != 0 {
		t.Fatalf("authorized keys after command revoke = %q", contents)
	}
}

func TestPairingRevokeCommandIsIdempotentAfterTrustWasAlreadyRemoved(t *testing.T) {
	root := t.TempDir()
	runtime := pairingRuntime{AuthorizedKeysPath: filepath.Join(root, "authorized_keys")}
	if err := os.WriteFile(runtime.AuthorizedKeysPath, nil, 0o600); err != nil {
		t.Fatalf("seed empty authorized keys: %v", err)
	}
	syncOperations := &recordingRemoteSync{revokeErr: errors.New("Syncthing identity is already absent")}
	if code := runPairingRevokeCommand(
		context.Background(), runtime, syncOperations, []string{"--device", "mac-device"}, &bytes.Buffer{},
	); code != 0 {
		t.Fatalf("idempotent runPairingRevokeCommand() code = %d", code)
	}
	if syncOperations.revoked != "" {
		t.Fatalf("already-complete revoke called Syncthing again for %q", syncOperations.revoked)
	}
}

func TestRPCDiagnosticsExposeTypedObservationAndExactRecoveryOnly(t *testing.T) {
	operations := &recordingRemoteDiagnostics{
		observation: remoteDiagnosticObservation{
			WSLRunning: true, SystemdTarget: true, DockerSocket: true,
			DiskAvailable: true, SyncthingService: true, PresenceActive: true,
		},
	}
	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":8,"method":"diagnostics.observe"}` + "\n" +
			`{"jsonrpc":"2.0","id":9,"method":"recovery.restart-systemd"}` + "\n" +
			`{"jsonrpc":"2.0","id":10,"method":"recovery.exec","params":{"command":"docker system prune"}}` + "\n",
	)
	var output bytes.Buffer
	if code := runRPCWithOperations(input, &output, &bytes.Buffer{}, pairingRuntime{}, operations); code != 0 {
		t.Fatalf("runRPCWithOperations() code = %d", code)
	}
	decoder := json.NewDecoder(&output)
	var observed response
	if err := decoder.Decode(&observed); err != nil {
		t.Fatalf("decode observation: %v", err)
	}
	if observed.Error != nil || observed.Result["wsl_running"] != true || observed.Result["systemd_target"] != true ||
		observed.Result["docker_socket"] != true || observed.Result["disk_available"] != true || observed.Result["syncthing_service"] != true || observed.Result["presence_active"] != true {
		t.Fatalf("observation response = %#v", observed)
	}
	var restarted response
	if err := decoder.Decode(&restarted); err != nil {
		t.Fatalf("decode restart: %v", err)
	}
	if restarted.Error != nil || restarted.Result["restarted"] != true || operations.restarts != 1 {
		t.Fatalf("restart response = %#v, calls=%d", restarted, operations.restarts)
	}
	var rejected response
	if err := decoder.Decode(&rejected); err != nil {
		t.Fatalf("decode rejected recovery: %v", err)
	}
	if rejected.Error == nil || rejected.Error.Code != -32601 {
		t.Fatalf("arbitrary recovery response = %#v", rejected)
	}
}

func TestDedicatedPresenceRPCAcceptsTypedHello(t *testing.T) {
	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":41,"method":"presence.hello","params":{"client_device_id":"mac","client_name":"MacBook","app_version":"0.2.5"}}` + "\n",
	)
	var output bytes.Buffer
	marker := &recordingPresenceMarker{}
	if code := runRPCWithPresenceMarker(input, &output, &bytes.Buffer{}, pairingRuntime{}, remoteSystemOperations{
		runner: staticSystemdRunner{}, freeBytes: func(string) (uint64, error) { return minimumDiagnosticFreeBytes, nil },
		probes: &recordingRemoteHealthProbes{docker: true, syncthing: true},
	}, nil, marker); code != 0 {
		t.Fatalf("presence command code = %d", code)
	}
	var hello response
	if err := json.NewDecoder(&output).Decode(&hello); err != nil {
		t.Fatal(err)
	}
	if hello.Error != nil || hello.Result["session_id"] == "" || hello.Result["docker_ready"] != true || hello.Result["sync_ready"] != true {
		t.Fatalf("hello response = %#v", hello)
	}
	if marker.activations != 1 || marker.deactivations != 1 {
		t.Fatalf("marker activations=%d deactivations=%d, want 1/1", marker.activations, marker.deactivations)
	}
}

type recordingPresenceMarker struct {
	activations   int
	deactivations int
	err           error
}

func (m *recordingPresenceMarker) Activate() error { m.activations++; return m.err }
func (m *recordingPresenceMarker) Deactivate()     { m.deactivations++ }

func TestRPCRuntimeStopContainersUsesOnlyTypedLifecycleOperation(t *testing.T) {
	operations := &recordingRemoteDiagnostics{}
	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":13,"method":"runtime.stop-containers"}` + "\n" +
			`{"jsonrpc":"2.0","id":14,"method":"runtime.exec","params":{"command":"docker system prune"}}` + "\n",
	)
	var output bytes.Buffer
	if code := runRPCWithOperations(input, &output, &bytes.Buffer{}, pairingRuntime{}, operations); code != 0 {
		t.Fatalf("runRPCWithOperations() code = %d", code)
	}
	decoder := json.NewDecoder(&output)
	var stopped, rejected response
	if err := decoder.Decode(&stopped); err != nil {
		t.Fatalf("decode stop response: %v", err)
	}
	if stopped.Error != nil || stopped.Result["stopped"] != true || operations.containerStops != 1 {
		t.Fatalf("stop response = %#v, calls=%d", stopped, operations.containerStops)
	}
	if err := decoder.Decode(&rejected); err != nil {
		t.Fatalf("decode rejected runtime operation: %v", err)
	}
	if rejected.Error == nil || rejected.Error.Code != -32601 {
		t.Fatalf("arbitrary runtime response = %#v", rejected)
	}
}

func TestRPCSyncMethodsAcceptOnlyTypedManagedOperations(t *testing.T) {
	syncOperations := &recordingRemoteSync{}
	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":20,"method":"sync.configure","params":{"device_id":"MAC-SYNC","folders":[{"id":"0123456789abcdef","path":"/Users/demo/project"}]}}` + "\n" +
			`{"jsonrpc":"2.0","id":21,"method":"sync.scan","params":{"folder_id":"0123456789abcdef"}}` + "\n" +
			`{"jsonrpc":"2.0","id":22,"method":"sync.status","params":{"folder_id":"0123456789abcdef","device_id":"MAC-SYNC"}}` + "\n" +
			`{"jsonrpc":"2.0","id":23,"method":"sync.exec","params":{"command":"rm -rf /"}}` + "\n",
	)
	var output bytes.Buffer
	if code := runRPCWithAllOperations(input, &output, &bytes.Buffer{}, pairingRuntime{}, nil, syncOperations); code != 0 {
		t.Fatalf("runRPCWithAllOperations() code = %d", code)
	}
	decoder := json.NewDecoder(&output)
	var configured, scanned, status, rejected response
	for _, destination := range []*response{&configured, &scanned, &status, &rejected} {
		if err := decoder.Decode(destination); err != nil {
			t.Fatalf("decode sync response: %v", err)
		}
	}
	if configured.Error != nil || configured.Result["configured"] != true || scanned.Error != nil || scanned.Result["scanned"] != true {
		t.Fatalf("configure=%#v scan=%#v", configured, scanned)
	}
	if status.Error != nil || status.Result["state"] != "idle" || status.Result["connected"] != true {
		t.Fatalf("status response = %#v", status)
	}
	if rejected.Error == nil || rejected.Error.Code != -32601 {
		t.Fatalf("arbitrary sync response = %#v", rejected)
	}
	if syncOperations.configure.DeviceID != "MAC-SYNC" || len(syncOperations.configure.Folders) != 1 ||
		syncOperations.scanned != "0123456789abcdef" || syncOperations.statusDevice != "MAC-SYNC" {
		t.Fatalf("recorded sync operations = %#v", syncOperations)
	}
}

func TestRemoteSystemOperationsUseExactAllowlistedCommands(t *testing.T) {
	tests := []struct {
		operation systemdOperation
		binary    string
		args      []string
	}{
		{operation: systemdTargetActive, binary: "/usr/bin/systemctl", args: []string{"is-active", "--quiet", "remote-docker.target"}},
		{operation: systemdTargetRestart, binary: "/usr/bin/sudo", args: []string{"--non-interactive", "/usr/bin/systemctl", "restart", "remote-docker.target"}},
	}
	for _, tt := range tests {
		binary, args, ok := systemdInvocation(tt.operation)
		if !ok || binary != tt.binary || !reflect.DeepEqual(args, tt.args) {
			t.Fatalf("%s invocation = (%q, %#v, %t), want (%q, %#v, true)", tt.operation, binary, args, ok, tt.binary, tt.args)
		}
	}
	if _, _, ok := systemdInvocation(systemdOperation("arbitrary")); ok {
		t.Fatal("arbitrary systemd operation received an invocation")
	}
}

func TestRPCDiagnosticsReturnStableErrorsWithoutCommandOutput(t *testing.T) {
	secret := "Authorization: Bearer remote-system-secret"
	operations := &recordingRemoteDiagnostics{observeErr: errors.New(secret), restartErr: errors.New(secret)}
	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":11,"method":"diagnostics.observe"}` + "\n" +
			`{"jsonrpc":"2.0","id":12,"method":"recovery.restart-systemd"}` + "\n",
	)
	var output bytes.Buffer
	if code := runRPCWithOperations(input, &output, &bytes.Buffer{}, pairingRuntime{}, operations); code != 0 {
		t.Fatalf("runRPCWithOperations() code = %d", code)
	}
	if strings.Contains(output.String(), "remote-system-secret") || strings.Contains(output.String(), secret) {
		t.Fatalf("RPC response leaked operation output: %s", &output)
	}
}

type recordingRemoteDiagnostics struct {
	observation    remoteDiagnosticObservation
	observeErr     error
	restartErr     error
	restarts       int
	containerStops int
}

type recordingRemoteSync struct {
	configure    remoteSyncConfigureParams
	scanned      string
	statusFolder string
	statusDevice string
	revoked      string
	revokeErr    error
}

func (r *recordingRemoteSync) Revoke(_ context.Context, deviceID string) error {
	r.revoked = deviceID
	return r.revokeErr
}

func (r *recordingRemoteSync) Configure(_ context.Context, params remoteSyncConfigureParams) error {
	r.configure = params
	return nil
}

func (r *recordingRemoteSync) Scan(_ context.Context, folderID string) error {
	r.scanned = folderID
	return nil
}

func (r *recordingRemoteSync) Status(_ context.Context, folderID, deviceID string) (remoteSyncStatus, error) {
	r.statusFolder = folderID
	r.statusDevice = deviceID
	return remoteSyncStatus{State: "idle", Connected: true}, nil
}

func (r *recordingRemoteDiagnostics) Observe(context.Context) (remoteDiagnosticObservation, error) {
	return r.observation, r.observeErr
}

func (r *recordingRemoteDiagnostics) RestartSystemdTarget(context.Context) error {
	r.restarts++
	return r.restartErr
}

func (r *recordingRemoteDiagnostics) StopContainers(context.Context) error {
	r.containerStops++
	return nil
}

func testAuthorizedLine(t *testing.T, deviceID string) string {
	t.Helper()
	return authorizedKey(t) + " " + managedPairingMarker + deviceID + "\n"
}

func authorizedKey(t *testing.T) string {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	sshKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatalf("NewPublicKey() error = %v", err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshKey)))
}

func TestRPCDoesNotEchoMalformedInput(t *testing.T) {
	secret := "private-token-value"
	input := strings.NewReader("{not-json-" + secret + "}\n")
	var output bytes.Buffer

	if code := runRPC(input, &output, &bytes.Buffer{}); code != 0 {
		t.Fatalf("runRPC() code = %d", code)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatalf("parse response leaked request content: %s", &output)
	}
}
