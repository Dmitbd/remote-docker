package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
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
