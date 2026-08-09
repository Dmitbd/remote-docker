//go:build !windows

package provision

import "os/exec"

func configureHiddenProcess(*exec.Cmd) {}
