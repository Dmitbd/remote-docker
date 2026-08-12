package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/credentials"
	"github.com/Dmitbd/remote-docker/internal/syncer"
)

const (
	localSyncthingCredentialOwner = "local-syncthing"
	localSyncthingEndpoint        = "http://127.0.0.1:8384"
	localSyncthingGUIAddress      = "127.0.0.1:8384"
	localSyncthingStopTimeout     = 5 * time.Second
)

type localSyncthingProcess interface {
	MarkReady() error
	Stop(context.Context) error
	Wait(context.Context) error
}

type localSyncthingOptions struct {
	Store               config.Store
	ConfigTransactions  *configTransactions
	Secrets             credentials.Store
	Executable          string
	PersistentConfigDir string
	DataDir             string
	RuntimeRoot         string
	Generator           syncer.IdentityGenerator
	Launcher            syncer.ProcessLauncher
	HTTPClient          *http.Client
	Bootstrap           func(context.Context, syncer.BootstrapOptions) (syncer.BootstrapResult, error)
	Start               func(context.Context, syncer.ProcessOptions) (localSyncthingProcess, error)
	WaitAPI             func(context.Context, *syncer.Client, time.Duration) error
}

type localSyncthingRuntime struct {
	options localSyncthingOptions
	mu      sync.Mutex
}

type localSyncPreparation struct {
	Recovered              bool
	CredentialCleanupError error
}

type localSyncIdentityBlockedError struct{ cause error }

func (e *localSyncIdentityBlockedError) Error() string {
	return "local Syncthing identity cannot be rotated while device state exists"
}

func (e *localSyncIdentityBlockedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func newLocalSyncthingRuntime(options localSyncthingOptions) *localSyncthingRuntime {
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: agentProbeTimeout}
	}
	if options.ConfigTransactions == nil {
		options.ConfigTransactions = newConfigTransactions(options.Store.Path)
	}
	return &localSyncthingRuntime{options: options}
}

// Prepare validates the stored local identity before any session-owned child
// can start. An unpaired corrupt identity is reset using public config as the
// durable boundary; paired or cleanup-dependent state remains fail-closed.
func (r *localSyncthingRuntime) Prepare(ctx context.Context) (localSyncPreparation, error) {
	if r == nil || r.options.Secrets == nil {
		return localSyncPreparation{}, errors.New("local Syncthing runtime is incomplete")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	cfg, err := loadAgentConfig(r.options.Store)
	if err != nil {
		return localSyncPreparation{}, fmt.Errorf("read local Syncthing configuration: %w", err)
	}
	if cfg.LocalSyncthingDeviceID == "" && len(cfg.LocalSyncthingIdentity) == 0 {
		return localSyncPreparation{}, nil
	}

	validationErr := error(nil)
	if strings.TrimSpace(cfg.LocalSyncthingDeviceID) == "" || len(cfg.LocalSyncthingIdentity) == 0 {
		validationErr = syncer.ErrIdentityCorrupt
	} else {
		key, keyErr := r.options.Secrets.Get(localSyncthingCredentialOwner, syncer.SyncthingIdentityKeyCredential)
		validationErr = keyErr
		if keyErr == nil {
			validationErr = syncer.ValidateEncryptedIdentity(cfg.LocalSyncthingIdentity, key)
			clear(key)
		}
	}
	if validationErr == nil {
		return localSyncPreparation{}, nil
	}
	if !errors.Is(validationErr, credentials.ErrNotFound) && !errors.Is(validationErr, syncer.ErrIdentityCorrupt) {
		return localSyncPreparation{}, fmt.Errorf("validate local Syncthing identity: %w", validationErr)
	}

	reset := false
	err = r.options.ConfigTransactions.RunContext(ctx, func() error {
		current, loadErr := loadAgentConfig(r.options.Store)
		if loadErr != nil {
			return loadErr
		}
		if current.ActiveDevice != "" || len(current.Devices) != 0 || len(current.PendingRevocations) != 0 {
			return &localSyncIdentityBlockedError{cause: validationErr}
		}
		if current.LocalSyncthingDeviceID != cfg.LocalSyncthingDeviceID ||
			!bytes.Equal(current.LocalSyncthingIdentity, cfg.LocalSyncthingIdentity) {
			return errors.New("local Syncthing identity changed during validation")
		}
		current.LocalSyncthingDeviceID = ""
		current.LocalSyncthingIdentity = nil
		if saveErr := r.options.Store.Save(current); saveErr != nil {
			return fmt.Errorf("reset local Syncthing identity: %w", saveErr)
		}
		reset = true
		return nil
	})
	if err != nil {
		return localSyncPreparation{}, err
	}
	if !reset {
		return localSyncPreparation{}, nil
	}

	cleanupErr := errors.Join(
		deleteCredentialIfPresent(r.options.Secrets, localSyncthingCredentialOwner, syncer.SyncthingIdentityKeyCredential),
		deleteCredentialIfPresent(r.options.Secrets, localSyncthingCredentialOwner, syncer.SyncthingAPIKeyCredential),
	)
	return localSyncPreparation{Recovered: true, CredentialCleanupError: cleanupErr}, nil
}

func deleteCredentialIfPresent(store credentials.Store, owner, name string) error {
	err := store.Delete(owner, name)
	if errors.Is(err, credentials.ErrNotFound) {
		return nil
	}
	return err
}

// DeviceID returns the stable local Syncthing identity, bootstrapping it once
// and persisting only its encrypted form in the non-secret config.
func (r *localSyncthingRuntime) DeviceID(ctx context.Context) (string, error) {
	if r == nil || r.options.Secrets == nil || strings.TrimSpace(r.options.Executable) == "" {
		return "", errors.New("local Syncthing runtime is incomplete")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var deviceID string
	err := r.options.ConfigTransactions.Run(func() error {
		cfg, err := loadAgentConfig(r.options.Store)
		if err != nil {
			return fmt.Errorf("read local Syncthing configuration: %w", err)
		}
		if cfg.LocalSyncthingDeviceID != "" || len(cfg.LocalSyncthingIdentity) != 0 {
			if strings.TrimSpace(cfg.LocalSyncthingDeviceID) == "" || len(cfg.LocalSyncthingIdentity) == 0 {
				return errors.New("local Syncthing identity state is incomplete")
			}
			deviceID = cfg.LocalSyncthingDeviceID
			return nil
		}

		bootstrap := r.options.Bootstrap
		if bootstrap == nil {
			bootstrap = syncer.BootstrapIdentity
		}
		result, err := bootstrap(ctx, syncer.BootstrapOptions{
			Executable: r.options.Executable, PersistentConfigDir: r.options.PersistentConfigDir,
			RuntimeRoot: r.options.RuntimeRoot, CredentialOwner: localSyncthingCredentialOwner,
			Secrets: r.options.Secrets, Generator: r.options.Generator,
		})
		if err != nil {
			return fmt.Errorf("bootstrap local Syncthing identity: %w", err)
		}
		if strings.TrimSpace(result.DeviceID) == "" || len(result.EncryptedIdentity) == 0 {
			return errors.New("bootstrap local Syncthing identity returned incomplete state")
		}
		cfg.SchemaVersion = config.CurrentSchemaVersion
		cfg.LocalSyncthingDeviceID = result.DeviceID
		cfg.LocalSyncthingIdentity = append([]byte(nil), result.EncryptedIdentity...)
		if err := r.options.Store.Save(cfg); err != nil {
			_ = r.options.Secrets.Delete(localSyncthingCredentialOwner, syncer.SyncthingIdentityKeyCredential)
			_ = r.options.Secrets.Delete(localSyncthingCredentialOwner, syncer.SyncthingAPIKeyCredential)
			return fmt.Errorf("persist local Syncthing identity: %w", err)
		}
		deviceID = result.DeviceID
		return nil
	})
	return deviceID, err
}

// Run owns the bundled Syncthing child for the background-agent lifetime.
func (r *localSyncthingRuntime) Run(ctx context.Context, interval time.Duration) error {
	if r == nil {
		return errors.New("local Syncthing runtime is nil")
	}
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	for {
		cfg, err := loadAgentConfig(r.options.Store)
		if err != nil {
			return fmt.Errorf("read local Syncthing configuration: %w", err)
		}
		if cfg.LocalSyncthingDeviceID == "" && len(cfg.LocalSyncthingIdentity) == 0 {
			if cfg.ActiveDevice == "" {
				if err := waitRuntimeInterval(ctx, interval); err != nil {
					return err
				}
				continue
			}
			if _, err := r.DeviceID(ctx); err != nil {
				return err
			}
			cfg, err = loadAgentConfig(r.options.Store)
			if err != nil {
				return fmt.Errorf("reload local Syncthing configuration: %w", err)
			}
		}
		if strings.TrimSpace(cfg.LocalSyncthingDeviceID) == "" || len(cfg.LocalSyncthingIdentity) == 0 {
			return errors.New("local Syncthing identity state is incomplete")
		}

		start := r.options.Start
		if start == nil {
			start = func(ctx context.Context, options syncer.ProcessOptions) (localSyncthingProcess, error) {
				return syncer.StartManagedProcess(ctx, options)
			}
		}
		process, err := start(ctx, syncer.ProcessOptions{
			Executable: r.options.Executable, PersistentConfigDir: r.options.PersistentConfigDir,
			DataDir: r.options.DataDir, RuntimeRoot: r.options.RuntimeRoot,
			GUIAddress: localSyncthingGUIAddress, DeviceID: localSyncthingCredentialOwner,
			Secrets: r.options.Secrets, EncryptedIdentity: append([]byte(nil), cfg.LocalSyncthingIdentity...),
			Launcher: r.options.Launcher,
		})
		if err != nil {
			return fmt.Errorf("start managed Syncthing: %w", err)
		}
		client, err := syncer.NewClient(localSyncthingEndpoint, localSyncthingCredentialOwner, r.options.Secrets, r.options.HTTPClient)
		if err != nil {
			_ = stopLocalSyncthing(process)
			return err
		}
		readyCtx, cancelReady := context.WithTimeout(ctx, startupRecoveryTimeout)
		waitAPI := r.options.WaitAPI
		if waitAPI == nil {
			waitAPI = waitLocalSyncthingAPI
		}
		err = waitAPI(readyCtx, client, interval)
		cancelReady()
		if err != nil {
			_ = stopLocalSyncthing(process)
			return fmt.Errorf("wait for managed Syncthing API: %w", err)
		}
		if err := process.MarkReady(); err != nil {
			_ = stopLocalSyncthing(process)
			return err
		}

		processDone := make(chan error, 1)
		go func() { processDone <- process.Wait(context.Background()) }()
		select {
		case <-ctx.Done():
			if err := stopLocalSyncthing(process); err != nil {
				return err
			}
			return ctx.Err()
		case err := <-processDone:
			if err == nil {
				return errors.New("managed Syncthing exited unexpectedly")
			}
			return fmt.Errorf("managed Syncthing exited: %w", err)
		}
	}
}

func waitLocalSyncthingAPI(ctx context.Context, client *syncer.Client, interval time.Duration) error {
	var lastErr error
	for {
		if _, err := client.Connections(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if err := waitRuntimeInterval(ctx, interval); err != nil {
			return fmt.Errorf("Syncthing API did not become ready: %v: %w", lastErr, err)
		}
	}
}

func waitRuntimeInterval(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func stopLocalSyncthing(process localSyncthingProcess) error {
	stopCtx, cancel := context.WithTimeout(context.Background(), localSyncthingStopTimeout)
	defer cancel()
	return process.Stop(stopCtx)
}
