//go:build windows

package watchdog

import (
	"context"
	"io"
	"os/exec"
	"syscall"
)

const (
	createNoWindow         = 0x08000000
	createBreakawayFromJob = 0x01000000
)

func configureChildProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true, CreationFlags: createNoWindow | createBreakawayFromJob,
	}
}

func cleanupOwnedResources(ctx context.Context) error {
	command := exec.CommandContext(ctx, "wsl.exe", "--terminate", "remote-docker")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return command.Run()
}
