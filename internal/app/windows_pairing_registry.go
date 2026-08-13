package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
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
	saveConfig         func(config.Config) error
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
	generation := strings.TrimSpace(peer.Generation)
	if deviceID == "" || generation == "" {
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
				RevocationProofHash: encodeRevocationProofHash(peer.RevocationProofHash),
				PairingGeneration:   generation,
			},
		}
		return r.save(cfg)
	})
}

func (r windowsPairingRegistry) RevokeWithProof(
	ctx context.Context,
	installer pairing.Installer,
	deviceID, generation string,
	proof []byte,
	afterSave ...func(context.Context, string, string) error,
) error {
	if installer == nil || len(proof) != pairing.RevocationProofSize {
		return errors.New("pairing revocation proof is invalid")
	}
	revoked := false
	err := r.runConfigTransaction(func() error {
		cfg, err := loadAgentConfig(r.store)
		if err != nil {
			return err
		}
		if cfg.ActiveDevice != deviceID {
			return nil
		}
		device, ok := cfg.Devices[deviceID]
		if !ok {
			return nil
		}
		if device.PairingGeneration == "" || generation == "" || device.PairingGeneration != generation {
			return nil
		}
		want, err := hex.DecodeString(device.RevocationProofHash)
		got := sha256.Sum256(proof)
		if err != nil || len(want) != sha256.Size || subtle.ConstantTimeCompare(want, got[:]) != 1 {
			return errors.New("pairing revocation proof is invalid")
		}
		if err := installer.Revoke(ctx, deviceID); err != nil {
			return err
		}
		cfg.ActiveDevice = ""
		cfg.Devices = nil
		if err := r.save(cfg); err != nil {
			return err
		}
		revoked = true
		return nil
	})
	if err != nil {
		return err
	}
	if revoked && len(afterSave) > 0 && afterSave[0] != nil {
		_ = afterSave[0](ctx, deviceID, generation)
	}
	return nil
}

func encodeRevocationProofHash(hash [32]byte) string {
	var zero [32]byte
	if subtle.ConstantTimeCompare(hash[:], zero[:]) == 1 {
		return ""
	}
	return hex.EncodeToString(hash[:])
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
		return r.save(cfg)
	})
}

func (r windowsPairingRegistry) save(cfg config.Config) error {
	if r.saveConfig != nil {
		return r.saveConfig(cfg)
	}
	return r.store.Save(cfg)
}

func (r windowsPairingRegistry) exactTrust(deviceID, generation string) (config.Device, bool, error) {
	var device config.Device
	found := false
	err := r.runConfigTransaction(func() error {
		cfg, err := loadAgentConfig(r.store)
		if err != nil {
			return err
		}
		candidate, ok := cfg.Devices[deviceID]
		if cfg.ActiveDevice == deviceID && ok && candidate.PairingGeneration == generation {
			device = candidate
			found = true
		}
		return nil
	})
	return device, found, err
}

func (r windowsPairingRegistry) runConfigTransaction(operation func() error) error {
	if r.configTransactions == nil {
		return newConfigTransactions(r.store.Path).Run(operation)
	}
	return r.configTransactions.Run(operation)
}
