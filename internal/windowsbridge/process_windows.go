//go:build windows

package windowsbridge

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

func configureHiddenProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
