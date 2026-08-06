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
		SchemaVersion: 1,
		ActiveDevice:  "pc-1",
		Devices: map[string]Device{
			"pc-1": {
				Name:     "Dev PC",
				Address:  "192.168.1.20",
				SSHPort:  2222,
				SyncPort: 22000,
			},
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

func TestStoreReplacesExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := Store{Path: path}

	if err := store.Save(Config{SchemaVersion: 1, ActiveDevice: "old"}); err != nil {
		t.Fatalf("first Save() error = %v", err)
	}
	want := Config{SchemaVersion: 1, ActiveDevice: "new"}
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
		SchemaVersion: 1,
		ActiveDevice:  "pc-1",
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
