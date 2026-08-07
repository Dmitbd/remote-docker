package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/Dmitbd/remote-docker/internal/pairing"
)

const defaultManagedDistro = "remote-docker"

// PairingCommand is an exact process invocation; no shell is involved.
type PairingCommand struct {
	Binary string
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// PairingCommandRunner executes the allowlisted WSL bridge command.
type PairingCommandRunner interface {
	Run(context.Context, PairingCommand) error
}

// WSLPairingInstaller updates only the managed WSL authorization through the
// bundled remote helper and returns its actual public identities.
type WSLPairingInstaller struct {
	Runner              PairingCommandRunner
	WSLBinary           string
	Distro              string
	SSHBridgePort       int
	SyncthingBridgePort int
}

func (i WSLPairingInstaller) Install(ctx context.Context, deviceID, authorizedKey string) (pairing.DeviceInfo, error) {
	if !validPairingDeviceID(deviceID) || !validManagedAuthorizedKey(authorizedKey) {
		return pairing.DeviceInfo{}, errors.New("managed pairing identity is invalid")
	}
	var output bytes.Buffer
	err := i.run(ctx, []string{"pairing-install", "--device", deviceID}, strings.NewReader(authorizedKey+"\n"), &output)
	if err != nil {
		return pairing.DeviceInfo{}, fmt.Errorf("install managed pairing key: %w", err)
	}
	var device pairing.DeviceInfo
	decoder := json.NewDecoder(io.LimitReader(&output, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&device); err != nil {
		return pairing.DeviceInfo{}, errors.New("decode managed pairing identities")
	}
	if strings.TrimSpace(device.SSHHostPublicKey) == "" || strings.TrimSpace(device.SyncthingDeviceID) == "" {
		return pairing.DeviceInfo{}, errors.New("managed pairing identities are incomplete")
	}
	device.SSHPort = i.SSHBridgePort
	if device.SSHPort == 0 {
		device.SSHPort = 49222
	}
	device.SyncthingPort = i.SyncthingBridgePort
	if device.SyncthingPort == 0 {
		device.SyncthingPort = 49220
	}
	return device, nil
}

func (i WSLPairingInstaller) Revoke(ctx context.Context, deviceID string) error {
	if !validPairingDeviceID(deviceID) {
		return errors.New("managed pairing identity is invalid")
	}
	if err := i.run(ctx, []string{"pairing-revoke", "--device", deviceID}, nil, io.Discard); err != nil {
		return fmt.Errorf("revoke managed pairing key: %w", err)
	}
	return nil
}

func (i WSLPairingInstaller) run(ctx context.Context, operation []string, stdin io.Reader, stdout io.Writer) error {
	runner := i.Runner
	if runner == nil {
		runner = execPairingCommandRunner{}
	}
	binary := i.WSLBinary
	if binary == "" {
		binary = "wsl.exe"
	}
	distro := i.Distro
	if distro == "" {
		distro = defaultManagedDistro
	}
	arguments := []string{
		"--distribution", distro,
		"--user", "remote-docker",
		"--exec", "/usr/local/bin/remote-docker-remote",
	}
	arguments = append(arguments, operation...)
	return runner.Run(ctx, PairingCommand{
		Binary: binary, Args: arguments, Stdin: stdin, Stdout: stdout, Stderr: io.Discard,
	})
}

func validPairingDeviceID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func validManagedAuthorizedKey(value string) bool {
	return strings.HasPrefix(value, "ssh-ed25519 ") && len(value) <= 16<<10 && !strings.ContainsAny(value, "\r\n")
}

type execPairingCommandRunner struct{}

func (execPairingCommandRunner) Run(ctx context.Context, command PairingCommand) error {
	process := exec.CommandContext(ctx, command.Binary, command.Args...)
	process.Stdin = command.Stdin
	process.Stdout = command.Stdout
	process.Stderr = command.Stderr
	return process.Run()
}

var _ pairing.Installer = WSLPairingInstaller{}
