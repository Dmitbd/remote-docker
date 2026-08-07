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

	"golang.org/x/crypto/ssh"
)

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
	if code := runRPCWithRuntime(input, &output, &bytes.Buffer{}, runtime); code != 0 {
		t.Fatalf("runRPCWithRuntime() code = %d", code)
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
}

func TestRPCDiagnosticsExposeTypedObservationAndExactRecoveryOnly(t *testing.T) {
	operations := &recordingRemoteDiagnostics{
		observation: remoteDiagnosticObservation{WSLRunning: true, SystemdTarget: true, DiskAvailable: true},
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
	if observed.Error != nil || observed.Result["wsl_running"] != true || observed.Result["systemd_target"] != true || observed.Result["disk_available"] != true {
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
	observation remoteDiagnosticObservation
	observeErr  error
	restartErr  error
	restarts    int
}

func (r *recordingRemoteDiagnostics) Observe(context.Context) (remoteDiagnosticObservation, error) {
	return r.observation, r.observeErr
}

func (r *recordingRemoteDiagnostics) RestartSystemdTarget(context.Context) error {
	r.restarts++
	return r.restartErr
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
