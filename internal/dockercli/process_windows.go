//go:build windows

package dockercli

import (
	"os"
	"os/exec"
)

func prepareCommand(_ *exec.Cmd) {}

func interruptProcess(process *os.Process) error {
	if err := process.Signal(os.Interrupt); err == nil {
		return nil
	}
	return process.Kill()
}
