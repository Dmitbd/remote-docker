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
		HostName:       "127.0.0.1",
		Port:           49222,
		AgentSocket:    "/tmp/rd-agent.sock",
		KnownHostsFile: "/tmp/rd-known-hosts",
		ControlDir:     "/tmp/rd-control",
	}

	got, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}
	want := `Host remote-docker-device-A1B2C3
  HostName 127.0.0.1
  Port 49222
  User remote-docker
  IdentityAgent /tmp/rd-agent.sock
  IdentityFile none
  StrictHostKeyChecking yes
  UserKnownHostsFile /tmp/rd-known-hosts
  HostKeyAlias remote-docker-device-A1B2C3
  PasswordAuthentication no
  KbdInteractiveAuthentication no
  BatchMode yes
  ControlMaster auto
  ControlPersist 60
  ControlPath /tmp/rd-control/%C
Host remote-docker-device-A1B2C3-control
  HostName 127.0.0.1
  Port 49223
  User remote-docker
  IdentityAgent /tmp/rd-agent.sock
  IdentityFile none
  StrictHostKeyChecking yes
  UserKnownHostsFile /tmp/rd-known-hosts
  HostKeyAlias remote-docker-device-A1B2C3
  PasswordAuthentication no
  KbdInteractiveAuthentication no
  BatchMode yes
  ControlMaster auto
  ControlPersist 60
  ControlPath /tmp/rd-control/%C
Host remote-docker-device-A1B2C3-metrics
  HostName 127.0.0.1
  Port 49224
  User remote-docker
  IdentityAgent /tmp/rd-agent.sock
  IdentityFile none
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
		"IdentitiesOnly yes",
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
		HostName:       "127.0.0.1",
		Port:           49222,
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

func TestRemovePinnedHostRemovesOnlyExactManagedAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	alias := "remote-docker-device-A1B2C3"
	content := "github.com ssh-ed25519 github-key\n" +
		alias + " " + authorizedPublicKey(t) + "\n" +
		alias + "-backup ssh-ed25519 backup-key\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed known_hosts: %v", err)
	}

	if err := RemovePinnedHost(path, alias); err != nil {
		t.Fatalf("RemovePinnedHost() error = %v", err)
	}
	want := "github.com ssh-ed25519 github-key\n" + alias + "-backup ssh-ed25519 backup-key\n"
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != want {
		t.Fatalf("known_hosts after removal = %q, want %q", got, want)
	}
}

func TestManagedRootRemoveConfigDeletesOnlyExactManagedChild(t *testing.T) {
	root := t.TempDir()
	managedRootPath := filepath.Join(root, "managed")
	unrelatedRootPath := filepath.Join(root, "unrelated")
	for _, directory := range []string{managedRootPath, unrelatedRootPath} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatalf("create %s: %v", directory, err)
		}
	}
	managedRoot, err := NewManagedRoot(managedRootPath)
	if err != nil {
		t.Fatalf("NewManagedRoot() error = %v", err)
	}
	managedPath := managedRoot.SSHConfigPath()
	unrelatedPath := filepath.Join(unrelatedRootPath, "ssh_config")
	for _, path := range []string{managedPath, unrelatedPath} {
		if err := os.WriteFile(path, []byte("config\n"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}

	if err := managedRoot.RemoveConfig(unrelatedPath); err == nil {
		t.Fatal("RemoveConfig(unrelated absolute ssh_config) error = nil")
	}
	if _, err := os.Stat(unrelatedPath); err != nil {
		t.Fatalf("unrelated config changed: %v", err)
	}
	if err := managedRoot.RemoveConfig(managedPath); err != nil {
		t.Fatalf("RemoveConfig(managed path) error = %v", err)
	}
	if _, err := os.Stat(managedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed config after removal error = %v", err)
	}
	if err := managedRoot.RemoveConfig(managedPath); err != nil {
		t.Fatalf("RemoveConfig(missing managed path) error = %v", err)
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
