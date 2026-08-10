package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	store := Store{Path: path}
	want := Config{
		SchemaVersion:          CurrentSchemaVersion,
		ActiveDevice:           "pc-1",
		LocalSyncthingDeviceID: "LOCAL-SYNC-DEVICE",
		LocalSyncthingIdentity: []byte("encrypted-identity"),
		Devices: map[string]Device{
			"pc-1": {
				Name:              "Dev PC",
				Address:           "192.168.1.20",
				SSHPort:           2222,
				SyncPort:          22000,
				SSHHostPublicKey:  "ssh-ed25519 AAAAhost",
				SyncthingDeviceID: "SYNC-DEVICE",
			},
		},
		Workspaces: map[string]Workspace{
			"sample": {Path: "/Users/demo/sample"},
		},
	}

	if err := store.Save(want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat() error = %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("config mode = %o, want 600", got)
		}
	}
}

func TestStoreLoadsConfigWithoutWorkspaceAndPairingMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := `{
  "schemaVersion": 1,
  "activeDevice": "pc-1",
  "devices": {
    "pc-1": {"name": "Dev PC", "address": "192.168.1.20", "sshPort": 2222}
  }
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	got, err := (Store{Path: path}).Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.ActiveDevice != "pc-1" || got.Devices["pc-1"].Name != "Dev PC" {
		t.Fatalf("Load() = %#v", got)
	}
	if got.Workspaces != nil || got.Devices["pc-1"].SSHHostPublicKey != "" || got.Devices["pc-1"].SyncthingDeviceID != "" {
		t.Fatalf("legacy optional fields were not zero-valued: %#v", got)
	}
	if got.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("legacy schema version = %d, want %d", got.SchemaVersion, CurrentSchemaVersion)
	}
}

func TestStoreMigratesV1ToOneTrustedDeviceAndPreservesWorkspaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := `{
  "schemaVersion": 1,
  "activeDevice": "pc-1",
  "devices": {
    "pc-1": {"name": "Trusted PC", "address": "192.168.1.20", "sshPort": 2222},
    "pc-old": {"name": "Old PC", "address": "192.168.1.30", "sshPort": 2222}
  },
  "workspaces": {"sample": {"path": "/Users/demo/sample"}}
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	got, migration, err := (Store{Path: path}).LoadWithMigration()
	if err != nil {
		t.Fatalf("LoadWithMigration() error = %v", err)
	}
	if got.SchemaVersion != CurrentSchemaVersion || got.ActiveDevice != "pc-1" || len(got.Devices) != 1 {
		t.Fatalf("migrated config = %#v", got)
	}
	if _, ok := got.Devices["pc-1"]; !ok {
		t.Fatalf("active device was not preserved: %#v", got.Devices)
	}
	if got.Workspaces["sample"].Path != "/Users/demo/sample" {
		t.Fatalf("workspace was not preserved: %#v", got.Workspaces)
	}
	if migration.FromVersion != 1 || migration.ToVersion != CurrentSchemaVersion || !reflect.DeepEqual(migration.RemovedDeviceIDs, []string{"pc-old"}) {
		t.Fatalf("migration = %#v", migration)
	}
}

func TestStoreRejectsFutureSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":999}`), 0o600); err != nil {
		t.Fatalf("write future config: %v", err)
	}

	_, err := (Store{Path: path}).Load()
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("Load() error = %v, want unsupported schema", err)
	}
}

func TestStoreRejectsMoreThanOneTrustedDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := (Store{Path: path}).Save(Config{
		SchemaVersion: CurrentSchemaVersion,
		ActiveDevice:  "pc-1",
		Devices: map[string]Device{
			"pc-1": {Name: "Trusted PC"},
			"pc-2": {Name: "Second PC"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "one trusted device") {
		t.Fatalf("Save() error = %v, want one-device validation", err)
	}
}

func TestStoreReplacesExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := Store{Path: path}

	if err := store.Save(Config{SchemaVersion: CurrentSchemaVersion}); err != nil {
		t.Fatalf("first Save() error = %v", err)
	}
	want := Config{SchemaVersion: CurrentSchemaVersion, Workspaces: map[string]Workspace{"new": {Path: "/tmp/new"}}}
	if err := store.Save(want); err != nil {
		t.Fatalf("second Save() error = %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".config.json.tmp-*"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files remain: %v", matches)
	}
}

func TestConfigJSONContainsNoSecretFields(t *testing.T) {
	data, err := json.Marshal(Config{
		SchemaVersion:          CurrentSchemaVersion,
		ActiveDevice:           "pc-1",
		LocalSyncthingIdentity: []byte("encrypted-identity"),
		Devices: map[string]Device{
			"pc-1": {Name: "Dev PC", Address: "192.168.1.20", SSHPort: 2222},
		},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	serialized := strings.ToLower(string(data))
	for _, forbidden := range []string{"privatekey", "private_key", "token", "password"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("serialized config contains forbidden field %q: %s", forbidden, data)
		}
	}
}

func TestDefaultPath(t *testing.T) {
	tests := []struct {
		name string
		goos string
		home string
		want string
	}{
		{
			name: "macOS",
			goos: "darwin",
			home: "/Users/demo",
			want: "/Users/demo/Library/Application Support/RemoteDocker/config.json",
		},
		{
			name: "Windows",
			goos: "windows",
			home: `C:\Users\demo`,
			want: `C:\Users\demo\AppData\Local\RemoteDocker\config.json`,
		},
		{
			name: "Linux",
			goos: "linux",
			home: "/home/demo",
			want: "/home/demo/.config/remote-docker/config.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DefaultPath(tt.goos, tt.home); got != tt.want {
				t.Fatalf("DefaultPath(%q, %q) = %q, want %q", tt.goos, tt.home, got, tt.want)
			}
		})
	}
}
