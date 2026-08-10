package sshtransport

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config describes one isolated SSH host entry.
type Config struct {
	DeviceID       string
	HostName       string
	Port           int
	AgentSocket    string
	KnownHostsFile string
	ControlDir     string
}

// ManagedRoot is an explicit capability for files owned by Remote Docker's
// isolated SSH runtime. It is established before any cleanup target is chosen.
type ManagedRoot struct {
	root          string
	sshConfigPath string
}

func NewManagedRoot(path string) (ManagedRoot, error) {
	if !filepath.IsAbs(path) {
		return ManagedRoot{}, errors.New("managed SSH root must be absolute")
	}
	root := filepath.Clean(path)
	if root == filepath.VolumeName(root)+string(filepath.Separator) {
		return ManagedRoot{}, errors.New("managed SSH root cannot be a filesystem root")
	}
	return ManagedRoot{root: root, sshConfigPath: filepath.Join(root, "ssh_config")}, nil
}

func (r ManagedRoot) SSHConfigPath() string {
	return r.sshConfigPath
}

// RenderConfig renders a strict SSH configuration without password fallbacks.
func RenderConfig(config Config) (string, error) {
	alias, err := hostAlias(config.DeviceID)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(config.HostName)
	if ip == nil || !ip.IsLoopback() || ip.To4() == nil {
		return "", errors.New("SSH host must be IPv4 loopback")
	}
	if config.Port != 49222 {
		return "", errors.New("Docker SSH alias must use loopback port 49222")
	}
	for name, path := range map[string]string{
		"agent socket":     config.AgentSocket,
		"known hosts file": config.KnownHostsFile,
		"control dir":      config.ControlDir,
	} {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("%s must be an absolute path", name)
		}
	}

	common := []string{
		"  User remote-docker",
		"  IdentityAgent " + sshToken(config.AgentSocket),
		"  IdentityFile none",
		"  StrictHostKeyChecking yes",
		"  UserKnownHostsFile " + sshToken(config.KnownHostsFile),
		"  HostKeyAlias " + alias,
		"  PasswordAuthentication no",
		"  KbdInteractiveAuthentication no",
		"  BatchMode yes",
		"  ControlMaster auto",
		"  ControlPersist 60",
		"  ControlPath " + sshToken(filepath.Join(config.ControlDir, "%C")),
	}
	lines := make([]string, 0, 3*(len(common)+3))
	for _, endpoint := range []struct {
		alias string
		port  int
	}{
		{alias, 49222},
		{alias + "-control", 49223},
		{alias + "-metrics", 49224},
	} {
		lines = append(lines, "Host "+endpoint.alias, "  HostName 127.0.0.1", "  Port "+strconv.Itoa(endpoint.port))
		lines = append(lines, common...)
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func DockerAlias(deviceID string) (string, error) { return hostAlias(deviceID) }

func ControlAlias(deviceID string) (string, error) {
	alias, err := hostAlias(deviceID)
	return alias + "-control", err
}

func MetricsAlias(deviceID string) (string, error) {
	alias, err := hostAlias(deviceID)
	return alias + "-metrics", err
}

// WriteConfig atomically writes a private app-managed SSH config file.
func WriteConfig(path string, config Config) error {
	content, err := RenderConfig(config)
	if err != nil {
		return err
	}
	return writePrivateFile(path, []byte(content))
}

// RemoveConfig deletes only the exact SSH config child authorized when the
// managed-root capability was created.
func (r ManagedRoot) RemoveConfig(path string) error {
	if r.root == "" || r.sshConfigPath == "" {
		return errors.New("managed SSH root capability is unavailable")
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != r.sshConfigPath {
		return errors.New("SSH config path is outside the managed SSH root")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove managed SSH config: %w", err)
	}
	return nil
}

func hostAlias(deviceID string) (string, error) {
	if deviceID == "" || len(deviceID) > 64 {
		return "", errors.New("invalid SSH device ID")
	}
	for _, character := range deviceID {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' {
			continue
		}
		return "", errors.New("invalid SSH device ID")
	}
	return "remote-docker-device-" + deviceID, nil
}

func sshToken(value string) string {
	if !strings.ContainsAny(value, " \t\r\n\"") {
		return value
	}
	escaped := strings.ReplaceAll(value, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	return "\"" + escaped + "\""
}

func writePrivateFile(path string, content []byte) (returnErr error) {
	if !filepath.IsAbs(path) {
		return errors.New("managed SSH file path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create managed SSH directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary managed SSH file: %w", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := temporary.Close(); returnErr == nil && closeErr != nil {
				returnErr = closeErr
			}
		}
		if returnErr != nil {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("set managed SSH file permissions: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		return fmt.Errorf("write managed SSH file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync managed SSH file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close managed SSH file: %w", err)
	}
	closed = true
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace managed SSH file: %w", err)
	}
	return nil
}
