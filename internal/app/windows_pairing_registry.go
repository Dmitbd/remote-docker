package app

import (
	"context"
	"errors"
	"strings"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/pairing"
	"github.com/Dmitbd/remote-docker/internal/tunnel"
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

func (r windowsPairingRegistry) Commit(_ context.Context, peer pairing.TrustedPeer) error {
	deviceID := strings.TrimSpace(peer.DeviceID)
	if deviceID == "" {
		return errors.New("trusted device ID is required")
	}
	encodedPeer := tunnel.EncodePublicKey(peer.PublicKey)
	if encodedPeer == "" {
		return errors.New("trusted tunnel public key is invalid")
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
			deviceID: {
				Name: "Mac", ClientDeviceID: deviceID, TunnelPort: tunnel.TunnelPort,
				TunnelPeerPublicKey: encodedPeer, TransportVersion: tunnel.CurrentTransportVersion,
			},
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
