package provision

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/Dmitbd/remote-docker/internal/credentials"
)

type managedRuntimeInstaller interface {
	Install(context.Context) error
}

type managedIdentityPreparer interface {
	Prepare(context.Context) error
}

// ManagedWSLRuntime orders packaged runtime installation before private
// identity materialization and keeps the latter recoverable while WSL runs.
type ManagedWSLRuntime struct {
	Installer managedRuntimeInstaller
	Identity  managedIdentityPreparer
}

func NewManagedWSLRuntime(executablePath string, secrets credentials.Store) (ManagedWSLRuntime, error) {
	if !filepath.IsAbs(executablePath) || filepath.Clean(executablePath) != executablePath || secrets == nil {
		return ManagedWSLRuntime{}, errors.New("managed WSL runtime options are invalid")
	}
	assetsRoot := filepath.Join(filepath.Dir(executablePath), "assets")
	candidatePath := filepath.Join(assetsRoot, "remote-docker-remote-linux-amd64")
	return ManagedWSLRuntime{
		Installer: WSLRuntimeInstaller{
			CandidatePath: candidatePath,
			ChecksumPath:  candidatePath + ".sha256",
		},
		Identity: WSLRuntimeIdentityPreparer{Secrets: secrets},
	}, nil
}

func (r ManagedWSLRuntime) Prepare(ctx context.Context) error {
	if r.Installer == nil || r.Identity == nil {
		return errors.New("managed WSL runtime is incomplete")
	}
	if err := r.Installer.Install(ctx); err != nil {
		return err
	}
	return r.Identity.Prepare(ctx)
}

func (r ManagedWSLRuntime) Run(ctx context.Context, interval time.Duration) error {
	if r.Installer == nil || r.Identity == nil {
		return errors.New("managed WSL runtime is incomplete")
	}
	if interval <= 0 {
		interval = time.Second
	}
	installed := false
	for {
		if !installed {
			installed = r.Installer.Install(ctx) == nil
		}
		if installed {
			_ = r.Identity.Prepare(ctx)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

var _ interface {
	Run(context.Context, time.Duration) error
} = ManagedWSLRuntime{}
