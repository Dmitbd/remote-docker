package sshtransport

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestRenderConfigGolden(t *testing.T) {
	config := Config{
		DeviceID:       "A1B2C3",
		HostName:       "192.168.1.20",
		Port:           2222,
		AgentSocket:    "/tmp/rd-agent.sock",
		KnownHostsFile: "/tmp/rd-known-hosts",
		ControlDir:     "/tmp/rd-control",
	}

	got, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}
	want := `Host remote-docker-device-A1B2C3
  HostName 192.168.1.20
  Port 2222
  User remote-docker
  IdentitiesOnly yes
  IdentityAgent /tmp/rd-agent.sock
  StrictHostKeyChecking yes
  UserKnownHostsFile /tmp/rd-known-hosts
  HostKeyAlias remote-docker-device-A1B2C3
  PasswordAuthentication no
  KbdInteractiveAuthentication no
  BatchMode yes
  ControlMaster auto
  ControlPersist 60
  ControlPath /tmp/rd-control/%C
`
	if got != want {
		t.Fatalf("RenderConfig() =\n%s\nwant:\n%s", got, want)
	}
	for _, forbidden := range []string{
		"StrictHostKeyChecking no",
		"UserKnownHostsFile /dev/null",
		"PasswordAuthentication yes",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("config contains unsafe directive %q", forbidden)
		}
	}
}

func TestWriteConfigCreatesPrivateAtomicFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ssh_config")
	config := Config{
		DeviceID:       "device-1",
		HostName:       "10.0.0.20",
		Port:           22,
		AgentSocket:    filepath.Join(directory, "agent.sock"),
		KnownHostsFile: filepath.Join(directory, "known_hosts"),
		ControlDir:     filepath.Join(directory, "control"),
	}
	if err := WriteConfig(path, config); err != nil {
		t.Fatalf("WriteConfig() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config permissions = %o, want 600", got)
	}
	matches, err := filepath.Glob(filepath.Join(directory, ".ssh_config.tmp-*"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary config files remain: %#v", matches)
	}
}

func TestPinKnownHostRejectsChangedKeyWithoutOverwriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	alias := "remote-docker-device-A1B2C3"
	firstKey := authorizedPublicKey(t)
	secondKey := authorizedPublicKey(t)

	if err := PinKnownHost(path, alias, firstKey); err != nil {
		t.Fatalf("PinKnownHost(first) error = %v", err)
	}
	if err := PinKnownHost(path, alias, firstKey); err != nil {
		t.Fatalf("PinKnownHost(same key) error = %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(before) error = %v", err)
	}

	err = PinKnownHost(path, alias, secondKey)
	if !errors.Is(err, ErrHostKeyChanged) {
		t.Fatalf("PinKnownHost(changed key) error = %v, want ErrHostKeyChanged", err)
	}
	if !strings.Contains(err.Error(), "remove pairing and pair the device again") {
		t.Fatalf("changed-key error lacks recovery instruction: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(after) error = %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("known_hosts changed after mismatch:\nbefore=%s\nafter=%s", before, after)
	}
}

func authorizedPublicKey(t *testing.T) string {
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
