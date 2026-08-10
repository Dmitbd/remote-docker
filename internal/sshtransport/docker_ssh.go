package sshtransport

import (
	"errors"
	"path/filepath"
	"strings"
)

const DockerSSHConfigEnvironment = "REMOTE_DOCKER_SSH_CONFIG"

// DockerSSHInvocation validates the exact SSH grammar emitted by the pinned
// Docker CLI for an ssh:// context, then injects the app-managed config.
func DockerSSHInvocation(configPath string, args []string) (Command, error) {
	if !filepath.IsAbs(configPath) {
		return Command{}, errors.New("managed Docker SSH config path must be absolute")
	}
	if len(args) != 5 || args[0] != "-o ConnectTimeout=30" ||
		args[1] != "-T" || args[2] != "--" || args[4] != "docker system dial-stdio" {
		return Command{}, errors.New("unsupported Docker SSH invocation")
	}
	host := args[3]
	const prefix = "remote-docker-device-"
	if !strings.HasPrefix(host, prefix) {
		return Command{}, errors.New("Docker SSH host is not managed")
	}
	deviceID := strings.TrimPrefix(host, prefix)
	alias, err := hostAlias(deviceID)
	if err != nil || alias != host {
		return Command{}, errors.New("Docker SSH host is invalid")
	}
	managedArgs := make([]string, 0, len(args)+2)
	managedArgs = append(managedArgs, "-F", configPath)
	managedArgs = append(managedArgs, args...)
	return Command{Binary: "/usr/bin/ssh", Args: managedArgs}, nil
}

// ManagedDockerEnvironment preserves the caller environment while ensuring
// Docker can discover only the packaged SSH adapter first.
func ManagedDockerEnvironment(base []string, dockerCLIPath, configPath string) ([]string, error) {
	if !filepath.IsAbs(dockerCLIPath) || !filepath.IsAbs(configPath) {
		return nil, errors.New("managed Docker paths must be absolute")
	}
	shimDir := filepath.Join(filepath.Dir(dockerCLIPath), "ssh-bin")
	result := make([]string, 0, len(base)+2)
	pathValue := ""
	for _, item := range base {
		key, value, found := strings.Cut(item, "=")
		if !found {
			continue
		}
		switch key {
		case "PATH":
			pathValue = value
		case DockerSSHConfigEnvironment:
		default:
			result = append(result, item)
		}
	}
	managedPath := shimDir
	if pathValue != "" {
		managedPath += string(filepath.ListSeparator) + pathValue
	}
	result = append(result, "PATH="+managedPath, DockerSSHConfigEnvironment+"="+configPath)
	return result, nil
}
