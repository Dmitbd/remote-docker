package provision

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWSLPairingInstallerUsesExactAllowlistedCommandsWithoutShell(t *testing.T) {
	runner := &recordingPairingRunner{output: `{"ssh_host_public_key":"ssh-ed25519 WINDOWS-HOST","syncthing_device_id":"WINDOWS-SYNC"}`}
	installer := WSLPairingInstaller{Runner: runner, WSLBinary: "wsl.exe", Distro: "remote-docker"}

	device, err := installer.Install(context.Background(), "mac-device", "ssh-ed25519 MANAGED-MAC-KEY")
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if device.SSHHostPublicKey != "ssh-ed25519 WINDOWS-HOST" || device.SyncthingDeviceID != "WINDOWS-SYNC" ||
		device.SSHPort != 49222 || device.SyncthingPort != 49220 {
		t.Fatalf("device = %#v", device)
	}
	wantInstall := []string{
		"--distribution", "remote-docker", "--user", "remote-docker", "--exec",
		"/usr/local/bin/remote-docker-remote", "pairing-install", "--device", "mac-device",
	}
	if runner.binary != "wsl.exe" || !reflect.DeepEqual(runner.args, wantInstall) {
		t.Fatalf("install command = %q %#v", runner.binary, runner.args)
	}
	if runner.stdin != "ssh-ed25519 MANAGED-MAC-KEY\n" {
		t.Fatalf("install stdin = %q", runner.stdin)
	}

	if err := installer.Revoke(context.Background(), "mac-device"); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	wantRevoke := []string{
		"--distribution", "remote-docker", "--user", "remote-docker", "--exec",
		"/usr/local/bin/remote-docker-remote", "pairing-revoke", "--device", "mac-device",
	}
	if !reflect.DeepEqual(runner.args, wantRevoke) || runner.stdin != "" {
		t.Fatalf("revoke command = %#v stdin=%q", runner.args, runner.stdin)
	}
}

func TestRootfsKeepsManagedAuthorizationWritableOnlyByServiceUser(t *testing.T) {
	containerfile, err := os.ReadFile(filepath.Join("..", "..", "packaging", "wsl", "Containerfile"))
	if err != nil {
		t.Fatalf("read Containerfile: %v", err)
	}
	sshd, err := os.ReadFile(filepath.Join("..", "..", "packaging", "wsl", "etc", "ssh", "sshd_config.d", "remote-docker.conf"))
	if err != nil {
		t.Fatalf("read sshd config: %v", err)
	}
	sshService, err := os.ReadFile(filepath.Join("..", "..", "packaging", "wsl", "etc", "systemd", "system", "ssh.service.d", "remote-docker.conf"))
	if err != nil {
		t.Fatalf("read SSH service configuration: %v", err)
	}
	if !strings.Contains(string(containerfile), "install -m 0600 -o remote-docker -g remote-docker /dev/null /var/lib/remote-docker/authorized_keys") {
		t.Fatalf("Containerfile does not create service-user managed authorized_keys")
	}
	if !strings.Contains(string(sshd), "AuthorizedKeysFile /var/lib/remote-docker/authorized_keys") {
		t.Fatalf("sshd does not use the service-user managed authorized_keys")
	}
	if !strings.Contains(string(sshd), "HostKey /etc/remote-docker/ssh_host_ed25519_key") ||
		!strings.Contains(string(sshService), "ssh-keygen -q -t ed25519") ||
		!strings.Contains(string(sshService), "/etc/remote-docker/ssh_host_ed25519_key") {
		t.Fatal("sshd does not generate and retain the public host identity returned during pairing")
	}
}

func TestRootfsAllowsOnlyExactManagedSystemdRecoveryElevation(t *testing.T) {
	sudoers, err := os.ReadFile(filepath.Join("..", "..", "packaging", "wsl", "etc", "sudoers.d", "remote-docker-recovery"))
	if err != nil {
		t.Fatalf("read recovery sudoers policy: %v", err)
	}
	policy := strings.TrimSpace(string(sudoers))
	want := "remote-docker ALL=(root) NOPASSWD: /usr/bin/systemctl restart remote-docker.target"
	if policy != want {
		t.Fatalf("recovery sudoers policy = %q, want exact allowlisted command %q", policy, want)
	}
	for _, forbidden := range []string{"ALL=(ALL)", "systemctl *", "/usr/bin/docker", "docker system", "/usr/bin/wsl", "NOPASSWD: ALL"} {
		if strings.Contains(policy, forbidden) {
			t.Fatalf("recovery sudoers policy contains broad capability %q: %s", forbidden, policy)
		}
	}
}

type recordingPairingRunner struct {
	output string
	binary string
	args   []string
	stdin  string
}

func (r *recordingPairingRunner) Run(_ context.Context, command PairingCommand) error {
	r.binary = command.Binary
	r.args = append([]string(nil), command.Args...)
	r.stdin = ""
	if command.Stdin != nil {
		contents, _ := io.ReadAll(command.Stdin)
		r.stdin = string(contents)
	}
	if command.Stdout != nil && strings.TrimSpace(r.output) != "" {
		_, _ = io.WriteString(command.Stdout, r.output)
	}
	return nil
}
