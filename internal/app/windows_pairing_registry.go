package app

import (
	"context"
	"errors"
	"strings"

	"github.com/Dmitbd/remote-docker/internal/config"
)

// windowsPairingRegistry persists only public metadata needed to enforce and
// render the single trusted Mac. The private SSH key remains on the Mac.
type windowsPairingRegistry struct {
	store              config.Store
	configTransactions *configTransactions
}

func (r windowsPairingRegistry) Allow(context.Context) error {
	cfg, err := loadAgentConfig(r.store)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.ActiveDevice) != "" {
		return errors.New("one trusted peer already exists")
	}
	return nil
}

func (r windowsPairingRegistry) Commit(_ context.Context, deviceID string) error {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return errors.New("trusted device ID is required")
	}
	return r.runConfigTransaction(func() error {
		cfg, err := loadAgentConfig(r.store)
		if err != nil {
			return err
		}
		if cfg.ActiveDevice != "" && cfg.ActiveDevice != deviceID {
			return errors.New("one trusted peer already exists")
		}
		cfg.SchemaVersion = config.CurrentSchemaVersion
		cfg.ActiveDevice = deviceID
		cfg.Devices = map[string]config.Device{
			deviceID: {Name: "Mac", ClientDeviceID: deviceID},
		}
		return r.store.Save(cfg)
	})
}

func (r windowsPairingRegistry) Forget(deviceID string) error {
	return r.runConfigTransaction(func() error {
		cfg, err := loadAgentConfig(r.store)
		if err != nil {
			return err
		}
		if deviceID == "" {
			deviceID = cfg.ActiveDevice
		}
		if cfg.ActiveDevice != deviceID {
			return errors.New("trusted device was not found")
		}
		cfg.ActiveDevice = ""
		cfg.Devices = nil
		return r.store.Save(cfg)
	})
}

func (r windowsPairingRegistry) runConfigTransaction(operation func() error) error {
	if r.configTransactions == nil {
		return operation()
	}
	return r.configTransactions.Run(operation)
}
