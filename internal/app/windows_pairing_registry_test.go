package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
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

func TestWindowsPairingRegistryRevokesWithPersistedCleanupProof(t *testing.T) {
	store := config.Store{Path: filepath.Join(t.TempDir(), "config.json")}
	registry := windowsPairingRegistry{store: store}
	proof := make([]byte, pairing.RevocationProofSize)
	if _, err := rand.Read(proof); err != nil {
		t.Fatalf("generate proof: %v", err)
	}
	peerKey := make(ed25519.PublicKey, ed25519.PublicKeySize)
	if err := registry.Commit(context.Background(), pairing.TrustedPeer{
		DeviceID: "mac-one", PublicKey: peerKey, RevocationProofHash: sha256.Sum256(proof),
	}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	installer := &runtimePairingInstaller{}
	wrongProof := append([]byte(nil), proof...)
	wrongProof[0] ^= 0xff
	if err := registry.RevokeWithProof(context.Background(), installer, "mac-one", wrongProof); err == nil {
		t.Fatal("RevokeWithProof() accepted the wrong proof")
	}
	unchanged, err := store.Load()
	if err != nil || unchanged.ActiveDevice != "mac-one" {
		t.Fatalf("wrong proof changed trust = %#v error=%v", unchanged, err)
	}
	if err := registry.RevokeWithProof(context.Background(), installer, "mac-one", proof); err != nil {
		t.Fatalf("RevokeWithProof() error = %v", err)
	}
	cfg, err := store.Load()
	if err != nil || cfg.ActiveDevice != "" || len(cfg.Devices) != 0 {
		t.Fatalf("config after revoke = %#v error=%v", cfg, err)
	}
}
