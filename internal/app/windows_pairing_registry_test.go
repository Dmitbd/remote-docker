package app

import (
	"context"
	"crypto/ed25519"
	"path/filepath"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/pairing"
	"github.com/Dmitbd/remote-docker/internal/tunnel"
)

func TestWindowsPairingRegistryEnforcesOnePublicTrustedPeer(t *testing.T) {
	store := config.Store{Path: filepath.Join(t.TempDir(), "config.json")}
	registry := windowsPairingRegistry{store: store}
	if err := registry.Allow(context.Background()); err != nil {
		t.Fatalf("Allow() before pairing error = %v", err)
	}
	peerKey := make(ed25519.PublicKey, ed25519.PublicKeySize)
	peerKey[0] = 1
	if err := registry.Commit(context.Background(), pairing.TrustedPeer{DeviceID: "mac-one", PublicKey: peerKey}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := registry.Allow(context.Background()); err == nil {
		t.Fatal("Allow() accepted a second trusted peer")
	}
	if err := registry.Commit(context.Background(), pairing.TrustedPeer{DeviceID: "mac-two", PublicKey: peerKey}); err == nil {
		t.Fatal("Commit() replaced the trusted peer")
	}
	cfg, err := store.Load()
	device := cfg.Devices["mac-one"]
	if err != nil || cfg.ActiveDevice != "mac-one" || len(cfg.Devices) != 1 || device.Name != "Mac" ||
		device.TunnelPort != tunnel.TunnelPort || device.TransportVersion != tunnel.CurrentTransportVersion ||
		device.TunnelPeerPublicKey != tunnel.EncodePublicKey(peerKey) {
		t.Fatalf("stored trust = %#v error=%v", cfg, err)
	}
	if err := registry.Forget("mac-one"); err != nil {
		t.Fatalf("Forget() error = %v", err)
	}
	if err := registry.Allow(context.Background()); err != nil {
		t.Fatalf("Allow() after forget error = %v", err)
	}
}
