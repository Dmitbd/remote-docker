package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Migration struct {
	FromVersion      int
	ToVersion        int
	RemovedDeviceIDs []string
}

// Store persists non-secret configuration as an atomic JSON file.
type Store struct {
	Path string
}

// Load reads and decodes the stored configuration.
func (s Store) Load() (Config, error) {
	cfg, _, err := s.LoadWithMigration()
	return cfg, err
}

// LoadWithMigration reads configuration and returns public identifiers for
// stale legacy device records. The application can use those identifiers to
// remove the corresponding owner-scoped credentials explicitly.
func (s Store) LoadWithMigration() (Config, Migration, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return Config{}, Migration{}, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, Migration{}, fmt.Errorf("decode config: %w", err)
	}
	migrated, migration, err := migrate(cfg)
	if err != nil {
		return Config{}, Migration{}, err
	}
	return migrated, migration, nil
}

// Save atomically replaces the stored configuration.
func (s Store) Save(cfg Config) error {
	if err := validate(cfg); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	temp, err := os.CreateTemp(dir, "."+filepath.Base(s.Path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)

	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("protect temporary config: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync temporary config: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if err := os.Rename(tempPath, s.Path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}

	return nil
}

func migrate(cfg Config) (Config, Migration, error) {
	if cfg.SchemaVersion > CurrentSchemaVersion {
		return Config{}, Migration{}, fmt.Errorf("configuration schema version %d is unsupported", cfg.SchemaVersion)
	}
	if cfg.SchemaVersion < 1 {
		return Config{}, Migration{}, errors.New("configuration schema version is unsupported")
	}
	migration := Migration{FromVersion: cfg.SchemaVersion, ToVersion: CurrentSchemaVersion}
	if cfg.SchemaVersion == 1 {
		for id := range cfg.Devices {
			if id != cfg.ActiveDevice {
				migration.RemovedDeviceIDs = append(migration.RemovedDeviceIDs, id)
				delete(cfg.Devices, id)
			}
		}
		sort.Strings(migration.RemovedDeviceIDs)
		if cfg.ActiveDevice != "" {
			if _, ok := cfg.Devices[cfg.ActiveDevice]; !ok {
				cfg.ActiveDevice = ""
			}
		}
		if cfg.ActiveDevice == "" {
			cfg.Devices = nil
		}
	}
	if cfg.SchemaVersion < CurrentSchemaVersion {
		cfg.SchemaVersion = CurrentSchemaVersion
	}
	if err := validate(cfg); err != nil {
		return Config{}, Migration{}, fmt.Errorf("validate config: %w", err)
	}
	return cfg, migration, nil
}

func validate(cfg Config) error {
	if cfg.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("configuration schema version %d is unsupported", cfg.SchemaVersion)
	}
	if len(cfg.Devices) > 1 {
		return errors.New("configuration supports only one trusted device")
	}
	if len(cfg.PendingRevocations) > 16 {
		return errors.New("configuration contains too many pending revocations")
	}
	if cfg.ActiveDevice == "" {
		// A single dormant public record is tolerated while an old pairing is
		// being revoked. Only ActiveDevice is trusted by the application.
		return nil
	}
	if _, ok := cfg.Devices[cfg.ActiveDevice]; !ok {
		return errors.New("active device record is missing")
	}
	return nil
}

// DefaultPath returns the platform-specific configuration file location.
func DefaultPath(goos, home string) string {
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "RemoteDocker", "config.json")
	case "windows":
		return strings.TrimRight(home, `\/`) + `\AppData\Local\RemoteDocker\config.json`
	default:
		return filepath.Join(home, ".config", "remote-docker", "config.json")
	}
}
