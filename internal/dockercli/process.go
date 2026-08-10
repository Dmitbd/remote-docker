package dockercli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

const processStopTimeout = 5 * time.Second

// Invocation describes one child-process execution.
type Invocation struct {
	Binary string
	Args   []string
	Env    []string
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Runner executes commands while preserving their terminal streams.
type Runner struct{}

// Run starts the requested command and waits for it to finish.
func (Runner) Run(ctx context.Context, invocation Invocation) error {
	if invocation.Binary == "" {
		return errors.New("docker CLI binary is required")
	}

	command := exec.CommandContext(ctx, invocation.Binary, invocation.Args...)
	command.Env = invocation.Env
	command.Dir = invocation.Dir
	command.Stdin = invocation.Stdin
	command.Stdout = invocation.Stdout
	command.Stderr = invocation.Stderr
	prepareCommand(command)
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return interruptProcess(command.Process)
	}
	command.WaitDelay = processStopTimeout

	if err := command.Run(); err != nil {
		return fmt.Errorf("run %s: %w", invocation.Binary, err)
	}

	return nil
}

// ExitCode converts a process result into a shell-compatible exit code.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, context.Canceled) {
		return 130
	}

	var exitError interface{ ExitCode() int }
	if errors.As(err, &exitError) {
		if code := exitError.ExitCode(); code >= 0 {
			return code
		}
		return 130
	}

	return 1
}
