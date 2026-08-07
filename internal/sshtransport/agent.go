package sshtransport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const SSHPrivateKeyCredential = "ssh-private-key"

// Command describes one direct process invocation without a shell.
type Command struct {
	Binary string
	Args   []string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Process is the child process owned by ManagedAgent.
type Process interface {
	Kill() error
	Wait() error
}

// CommandRunner starts ssh-agent and synchronously runs ssh-add.
type CommandRunner interface {
	Start(ctx context.Context, command Command) (Process, error)
	Run(ctx context.Context, command Command) error
}

// Agent launches an app-owned agent and loads one key into it.
type Agent struct {
	Runner        CommandRunner
	AgentBinary   string
	AddBinary     string
	Env           []string
	Stdout        io.Writer
	Stderr        io.Writer
	WaitForSocket func(context.Context, string) error
}

// ManagedAgent owns only the child process started by Agent.
type ManagedAgent struct {
	process Process
	once    sync.Once
	err     error
}

// Start launches an isolated foreground ssh-agent and loads privateKey via stdin.
func (a Agent) Start(ctx context.Context, socketPath string, privateKey []byte) (*ManagedAgent, error) {
	defer clearBytes(privateKey)
	if len(privateKey) == 0 {
		return nil, errors.New("SSH private key is empty")
	}
	if !filepath.IsAbs(socketPath) || len(socketPath) > 100 {
		return nil, errors.New("managed ssh-agent socket path must be short and absolute")
	}
	parentInfo, err := os.Lstat(filepath.Dir(socketPath))
	if err != nil {
		return nil, fmt.Errorf("inspect managed ssh-agent directory: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("managed ssh-agent directory must be private")
	}
	if _, err := os.Lstat(socketPath); err == nil {
		return nil, errors.New("managed ssh-agent socket path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect managed ssh-agent socket: %w", err)
	}

	runner := a.Runner
	if runner == nil {
		runner = execCommandRunner{}
	}
	agentBinary := a.AgentBinary
	if agentBinary == "" {
		agentBinary = "ssh-agent"
	}
	addBinary := a.AddBinary
	if addBinary == "" {
		addBinary = "ssh-add"
	}
	environment := managedAgentEnv(a.Env, socketPath)
	process, err := runner.Start(ctx, Command{
		Binary: agentBinary,
		Args:   []string{"-D", "-a", socketPath},
		Env:    environment,
		Stdout: a.Stdout,
		Stderr: a.Stderr,
	})
	if err != nil {
		return nil, fmt.Errorf("start managed ssh-agent: %w", err)
	}
	managed := &ManagedAgent{process: process}
	waitForSocket := a.WaitForSocket
	if waitForSocket == nil {
		waitForSocket = waitForAgentSocket
	}
	if err := waitForSocket(ctx, socketPath); err != nil {
		_ = managed.Close()
		return nil, fmt.Errorf("wait for managed ssh-agent: %w", err)
	}
	if err := runner.Run(ctx, Command{
		Binary: addBinary,
		Args:   []string{"-"},
		Env:    environment,
		Stdin:  bytes.NewReader(privateKey),
		Stdout: a.Stdout,
		Stderr: a.Stderr,
	}); err != nil {
		_ = managed.Close()
		return nil, fmt.Errorf("load key into managed ssh-agent: %w", err)
	}
	return managed, nil
}

// Close terminates and waits for only the app-owned ssh-agent child.
func (a *ManagedAgent) Close() error {
	if a == nil || a.process == nil {
		return nil
	}
	a.once.Do(func() {
		killErr := a.process.Kill()
		waitErr := a.process.Wait()
		if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			a.err = killErr
		} else if waitErr != nil {
			var exitError *exec.ExitError
			if !errors.As(waitErr, &exitError) {
				a.err = waitErr
			}
		}
	})
	return a.err
}

func managedAgentEnv(base []string, socketPath string) []string {
	if base == nil {
		base = os.Environ()
	}
	environment := make([]string, 0, len(base)+1)
	for _, variable := range base {
		if strings.HasPrefix(variable, "SSH_AUTH_SOCK=") || strings.HasPrefix(variable, "SSH_AGENT_PID=") {
			continue
		}
		environment = append(environment, variable)
	}
	return append(environment, "SSH_AUTH_SOCK="+socketPath)
}

func waitForAgentSocket(ctx context.Context, socketPath string) error {
	waitContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := os.Stat(socketPath)
		if err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-waitContext.Done():
			return waitContext.Err()
		case <-ticker.C:
		}
	}
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

type execCommandRunner struct{}

func (execCommandRunner) Start(ctx context.Context, command Command) (Process, error) {
	process := exec.CommandContext(ctx, command.Binary, command.Args...)
	process.Env = command.Env
	process.Stdin = command.Stdin
	process.Stdout = command.Stdout
	process.Stderr = command.Stderr
	if err := process.Start(); err != nil {
		return nil, err
	}
	return &execProcess{command: process}, nil
}

func (execCommandRunner) Run(ctx context.Context, command Command) error {
	process := exec.CommandContext(ctx, command.Binary, command.Args...)
	process.Env = command.Env
	process.Stdin = command.Stdin
	process.Stdout = command.Stdout
	process.Stderr = command.Stderr
	return process.Run()
}

type execProcess struct {
	command *exec.Cmd
}

func (p *execProcess) Kill() error {
	return p.command.Process.Kill()
}

func (p *execProcess) Wait() error {
	return p.command.Wait()
}

var _ CommandRunner = execCommandRunner{}
var _ Process = (*execProcess)(nil)
