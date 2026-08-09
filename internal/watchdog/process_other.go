//go:build !darwin && !windows

package watchdog

import (
	"context"
	"os/exec"
)

func configureChildProcess(*exec.Cmd) {}
func cleanupOwnedResources(context.Context) error { return nil }
