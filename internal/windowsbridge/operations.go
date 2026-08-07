package windowsbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
)

const managedDistroName = "remote-docker"

// ManagedWSLStatus is the fixed, non-sensitive status subset used by
// diagnostics. It contains no command output or host paths.
type ManagedWSLStatus struct {
	Running       bool `json:"wsl_running"`
	SystemdTarget bool `json:"systemd_target"`
	DiskAvailable bool `json:"disk_available"`
}

type managedWSLOperation string

const (
	operationListRunning          managedWSLOperation = "list_running"
	operationObserve              managedWSLOperation = "observe"
	operationStartDistro          managedWSLOperation = "start_distro"
	operationRestartSystemdTarget managedWSLOperation = "restart_systemd_target"
)

type managedWSLRunner interface {
	Run(context.Context, managedWSLOperation, io.Reader, io.Writer) error
}

// ManagedWSLOperations owns only fixed operations for the managed distro. No
// caller can supply a command, argument, unit, distro, or shell fragment.
type ManagedWSLOperations struct {
	runner managedWSLRunner
}

// Observe checks whether the distro is already running before using its typed
// RPC. It never starts a stopped distro as a side effect of Doctor.
func (o ManagedWSLOperations) Observe(ctx context.Context) (ManagedWSLStatus, error) {
	runner := o.runner
	if runner == nil {
		runner = execManagedWSLRunner{}
	}
	var running bytes.Buffer
	if err := runner.Run(ctx, operationListRunning, nil, &running); err != nil {
		return ManagedWSLStatus{}, errors.New("read managed WSL running state")
	}
	names := strings.Fields(strings.ReplaceAll(running.String(), "\x00", ""))
	found := false
	for _, name := range names {
		if name == managedDistroName {
			found = true
			break
		}
	}
	if !found {
		return ManagedWSLStatus{}, nil
	}

	request, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "diagnostics.observe",
	})
	if err != nil {
		return ManagedWSLStatus{}, errors.New("encode managed WSL observation")
	}
	var output bytes.Buffer
	if err := runner.Run(ctx, operationObserve, bytes.NewReader(append(request, '\n')), &output); err != nil {
		return ManagedWSLStatus{}, errors.New("observe managed WSL environment")
	}
	var response struct {
		JSONRPC string           `json:"jsonrpc"`
		ID      int              `json:"id"`
		Result  ManagedWSLStatus `json:"result"`
		Error   json.RawMessage  `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(&output, 64<<10)).Decode(&response); err != nil ||
		response.JSONRPC != "2.0" || response.ID != 1 || len(response.Error) != 0 {
		return ManagedWSLStatus{}, errors.New("decode managed WSL observation")
	}
	response.Result.Running = true
	return response.Result, nil
}

// StartDistro starts only the managed distro by running its fixed health
// helper. It cannot execute a user command.
func (o ManagedWSLOperations) StartDistro(ctx context.Context) error {
	return o.runRecovery(ctx, operationStartDistro)
}

// RestartSystemdTarget restarts only remote-docker.target as root through the
// Windows-owned WSL control path.
func (o ManagedWSLOperations) RestartSystemdTarget(ctx context.Context) error {
	return o.runRecovery(ctx, operationRestartSystemdTarget)
}

func (o ManagedWSLOperations) runRecovery(ctx context.Context, operation managedWSLOperation) error {
	runner := o.runner
	if runner == nil {
		runner = execManagedWSLRunner{}
	}
	if err := runner.Run(ctx, operation, nil, io.Discard); err != nil {
		return errors.New("managed WSL recovery operation failed")
	}
	return nil
}

func managedWSLInvocation(operation managedWSLOperation) (string, []string, bool) {
	switch operation {
	case operationListRunning:
		return "wsl.exe", []string{"--list", "--running", "--quiet"}, true
	case operationObserve:
		return "wsl.exe", []string{
			"--distribution", managedDistroName, "--exec", "/usr/local/bin/remote-docker-remote", "rpc",
		}, true
	case operationStartDistro:
		return "wsl.exe", []string{
			"--distribution", managedDistroName, "--exec", "/usr/local/bin/remote-docker-remote", "health",
		}, true
	case operationRestartSystemdTarget:
		return "wsl.exe", []string{
			"--distribution", managedDistroName, "--user", "root", "--exec",
			"/usr/bin/systemctl", "restart", "remote-docker.target",
		}, true
	default:
		return "", nil, false
	}
}

type execManagedWSLRunner struct{}

func (execManagedWSLRunner) Run(ctx context.Context, operation managedWSLOperation, stdin io.Reader, stdout io.Writer) error {
	binary, args, ok := managedWSLInvocation(operation)
	if !ok {
		return errors.New("unsupported managed WSL operation")
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = io.Discard
	return command.Run()
}
