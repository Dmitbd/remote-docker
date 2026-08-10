package app

import (
	"errors"
	"path/filepath"

	"github.com/Dmitbd/remote-docker/internal/processowner"
	"github.com/Dmitbd/remote-docker/internal/watchdog"
)

func AttachProductionProcessOwner() (processowner.Owner, error) {
	return processowner.AttachCurrentProcess()
}

func ProductionWatchdogFactory(executable string) (WatchdogFactory, error) {
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return nil, errors.New("desktop watchdog executable path must be absolute and clean")
	}
	return func() (CrashWatchdog, error) { return watchdog.Start(executable) }, nil
}
