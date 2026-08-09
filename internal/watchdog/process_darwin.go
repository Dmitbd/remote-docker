//go:build darwin

package watchdog

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func configureChildProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func cleanupOwnedResources(context.Context) error {
	groupID := syscall.Getppid()
	if groupID <= 1 {
		return errors.New("owned process group is unavailable")
	}
	if err := syscall.Kill(-groupID, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return err
	}
	time.Sleep(5 * time.Second)
	if err := syscall.Kill(-groupID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
