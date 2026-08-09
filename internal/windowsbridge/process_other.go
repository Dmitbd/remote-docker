//go:build !windows

package windowsbridge

import "os/exec"

func configureHiddenProcess(*exec.Cmd) {}
