//go:build !windows

package dockercli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func prepareCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func interruptProcess(process *os.Process) error {
	err := syscall.Kill(-process.Pid, syscall.SIGINT)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
