package sshtransport

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDockerSSHInvocationAcceptsOnlyPinnedManagedDialStdioGrammar(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "ssh_config")
	command, err := DockerSSHInvocation(configPath, []string{
		"-o ConnectTimeout=30", "-T", "--",
		"remote-docker-device-A1B2C3", "docker system dial-stdio",
	})
	if err != nil {
		t.Fatalf("DockerSSHInvocation() error = %v", err)
	}
	if command.Binary != "/usr/bin/ssh" {
		t.Fatalf("binary = %q, want /usr/bin/ssh", command.Binary)
	}
	want := []string{
		"-F", configPath, "-o ConnectTimeout=30", "-T", "--",
		"remote-docker-device-A1B2C3", "docker system dial-stdio",
	}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("args = %#v, want %#v", command.Args, want)
	}
}

func TestDockerSSHInvocationRejectsOverridesAndArbitraryCommands(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "ssh_config")
	tests := [][]string{
		{"-F", "/tmp/attacker", "remote-docker-device-A1", "docker system dial-stdio"},
		{"-o StrictHostKeyChecking=no", "-T", "--", "remote-docker-device-A1", "docker system dial-stdio"},
		{"-o ConnectTimeout=30", "-T", "--", "example.com", "docker system dial-stdio"},
		{"-o ConnectTimeout=30", "-T", "--", "remote-docker-device-A1", "sh -c whoami"},
	}
	for _, args := range tests {
		if _, err := DockerSSHInvocation(configPath, args); err == nil {
			t.Fatalf("DockerSSHInvocation(%#v) error = nil", args)
		}
	}
}

func TestManagedDockerEnvironmentOwnsOnlySSHConfigAndPathPrefix(t *testing.T) {
	base := []string{"HOME=/Users/demo", "PATH=/opt/tools:/usr/bin", "REMOTE_DOCKER_SSH_CONFIG=/attacker"}
	got, err := ManagedDockerEnvironment(base, "/usr/local/libexec/remote-docker/docker-real", "/Users/demo/Library/Application Support/Remote Docker/ssh_config")
	if err != nil {
		t.Fatal(err)
	}
	if !containsEnvironment(got, "HOME=/Users/demo") {
		t.Fatalf("environment lost unrelated value: %#v", got)
	}
	if !containsEnvironment(got, "REMOTE_DOCKER_SSH_CONFIG=/Users/demo/Library/Application Support/Remote Docker/ssh_config") {
		t.Fatalf("managed config missing: %#v", got)
	}
	path := environmentValue(got, "PATH")
	if !strings.HasPrefix(path, "/usr/local/libexec/remote-docker/ssh-bin"+string(filepath.ListSeparator)) || !strings.HasSuffix(path, "/opt/tools:/usr/bin") {
		t.Fatalf("PATH = %q", path)
	}
}

func containsEnvironment(environment []string, value string) bool {
	for _, item := range environment {
		if item == value {
			return true
		}
	}
	return false
}

func environmentValue(environment []string, key string) string {
	for _, item := range environment {
		if strings.HasPrefix(item, key+"=") {
			return strings.TrimPrefix(item, key+"=")
		}
	}
	return ""
}
