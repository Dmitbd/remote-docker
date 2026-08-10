package main

import (
	"context"
	"errors"
	"os"

	"github.com/Dmitbd/remote-docker/internal/credentials"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/provision"
)

type maintenanceDependencies struct {
	prepareWSL          func(context.Context) error
	deleteWSLCredential func() error
	shutdownDesktop     func(context.Context) error
}

func runMaintenanceCommand(ctx context.Context, arguments []string, dependencies maintenanceDependencies) (bool, error) {
	if len(arguments) != 1 {
		return false, nil
	}
	switch arguments[0] {
	case "--prepare-wsl":
		if dependencies.prepareWSL == nil {
			return true, errors.New("prepare managed WSL runtime")
		}
		return true, dependencies.prepareWSL(ctx)
	case "--delete-wsl-credential":
		if dependencies.deleteWSLCredential == nil {
			return true, errors.New("delete managed WSL credential")
		}
		return true, dependencies.deleteWSLCredential()
	case "--shutdown":
		if dependencies.shutdownDesktop == nil {
			return true, errors.New("request desktop shutdown")
		}
		return true, dependencies.shutdownDesktop(ctx)
	default:
		return false, nil
	}
}

func productionMaintenanceDependencies() maintenanceDependencies {
	return maintenanceDependencies{
		prepareWSL: func(ctx context.Context) error {
			executablePath, err := os.Executable()
			if err != nil {
				return errors.New("locate desktop executable")
			}
			runtime, err := provision.NewManagedWSLRuntime(executablePath, credentials.NewKeyringStore())
			if err != nil {
				return err
			}
			return runtime.Prepare(ctx)
		},
		deleteWSLCredential: func() error {
			err := credentials.NewKeyringStore().Delete(
				provision.WindowsRuntimeCredentialOwner,
				provision.WindowsRuntimeIdentityKeyCredential,
			)
			if errors.Is(err, credentials.ErrNotFound) {
				return nil
			}
			return err
		},
		shutdownDesktop: func(ctx context.Context) error {
			var result map[string]any
			_ = (localapi.Client{}).Call(ctx, localapi.MethodShutdown, nil, &result)
			return nil
		},
	}
}
