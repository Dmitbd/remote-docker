package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/config"
)

func TestWindowsPairingRegistryEnforcesOnePublicTrustedPeer(t *testing.T) {
	store := config.Store{Path: filepath.Join(t.TempDir(), "config.json")}
	registry := windowsPairingRegistry{store: store}
	if err := registry.Allow(context.Background()); err != nil {
		t.Fatalf("Allow() before pairing error = %v", err)
	}
	if err := registry.Commit(context.Background(), "mac-one"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := registry.Allow(context.Background()); err == nil {
		t.Fatal("Allow() accepted a second trusted peer")
	}
	if err := registry.Commit(context.Background(), "mac-two"); err == nil {
		t.Fatal("Commit() replaced the trusted peer")
	}
	cfg, err := store.Load()
	if err != nil || cfg.ActiveDevice != "mac-one" || len(cfg.Devices) != 1 || cfg.Devices["mac-one"].Name != "Mac" {
		t.Fatalf("stored trust = %#v error=%v", cfg, err)
	}
	if err := registry.Forget("mac-one"); err != nil {
		t.Fatalf("Forget() error = %v", err)
	}
	if err := registry.Allow(context.Background()); err != nil {
		t.Fatalf("Allow() after forget error = %v", err)
	}
}
