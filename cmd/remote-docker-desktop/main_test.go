package main

import (
	"path/filepath"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/lifecycle"
)

func TestInitialTrustedPeerRestoresOnlyActivePublicRecord(t *testing.T) {
	store := config.Store{Path: filepath.Join(t.TempDir(), "config.json")}
	if peer := initialTrustedPeer(store, lifecycle.RoleMacClient); peer != nil {
		t.Fatalf("missing config peer = %#v", peer)
	}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion, ActiveDevice: "windows",
		Devices: map[string]config.Device{"windows": {Name: "Windows PC", Address: "192.168.1.20"}},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	peer := initialTrustedPeer(store, lifecycle.RoleMacClient)
	if peer == nil || peer.ID != "windows" || peer.Name != "Windows PC" || peer.OS != "windows" || peer.Address != "192.168.1.20" {
		t.Fatalf("restored peer = %#v", peer)
	}
}
