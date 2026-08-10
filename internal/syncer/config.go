package syncer

import (
	"errors"
	"strings"
)

// DefaultIgnores excludes generated or VCS-owned directories, but not .env.
var DefaultIgnores = []string{
	"(?d).git",
	"(?d)node_modules",
	"(?d).pnpm-store",
	"(?d).turbo",
	"(?d)__pycache__",
	"(?d).pytest_cache",
	"(?d).mypy_cache",
	"(?d).idea",
	"(?d).vscode",
	"(?d)dist",
	"(?d)build",
}

// DeviceConfig is the restricted paired-device subset sent to Syncthing.
type DeviceConfig struct {
	DeviceID          string   `json:"deviceID"`
	Name              string   `json:"name,omitempty"`
	Addresses         []string `json:"addresses"`
	Compression       string   `json:"compression"`
	Introducer        bool     `json:"introducer"`
	AutoAcceptFolders bool     `json:"autoAcceptFolders"`
}

// FolderDevice links one paired device to a folder.
type FolderDevice struct {
	DeviceID string `json:"deviceID"`
}

// FolderConfig fixes the workspace sync safety defaults.
type FolderConfig struct {
	ID               string         `json:"id"`
	Label            string         `json:"label,omitempty"`
	Path             string         `json:"path"`
	Type             string         `json:"type"`
	Devices          []FolderDevice `json:"devices"`
	FSWatcherEnabled bool           `json:"fsWatcherEnabled"`
	MaxConflicts     int            `json:"maxConflicts"`
}

// HardenedOptions disables autonomous external networking and telemetry.
type HardenedOptions struct {
	GlobalAnnounceEnabled bool `json:"globalAnnounceEnabled"`
	LocalAnnounceEnabled  bool `json:"localAnnounceEnabled"`
	RelaysEnabled         bool `json:"relaysEnabled"`
	StartBrowser          bool `json:"startBrowser"`
	URAccepted            int  `json:"urAccepted"`
	UpgradeToPreReleases  bool `json:"upgradeToPreReleases"`
}

// NewDeviceConfig restricts the remote to the authenticated local tunnel relay.
func NewDeviceConfig(deviceID, name string) (DeviceConfig, error) {
	if strings.TrimSpace(deviceID) == "" {
		return DeviceConfig{}, errors.New("Syncthing device ID is empty")
	}
	return DeviceConfig{
		DeviceID:    deviceID,
		Name:        name,
		Addresses:   []string{"tcp://127.0.0.1:49220"},
		Compression: "metadata",
	}, nil
}

// NewPassiveDeviceConfig permits only authenticated incoming connections from
// one already-paired device. With discovery and relays disabled, "dynamic"
// does not publish or discover an address.
func NewPassiveDeviceConfig(deviceID, name string) (DeviceConfig, error) {
	if strings.TrimSpace(deviceID) == "" || len(deviceID) > 128 || strings.TrimSpace(deviceID) != deviceID {
		return DeviceConfig{}, errors.New("Syncthing device ID is invalid")
	}
	return DeviceConfig{
		DeviceID: deviceID, Name: name, Addresses: []string{"dynamic"}, Compression: "metadata",
	}, nil
}

// NewFolderConfig returns a send-receive workspace with bounded conflicts.
func NewFolderConfig(id, remotePath, pairedDeviceID string) FolderConfig {
	return FolderConfig{
		ID:               id,
		Label:            id,
		Path:             remotePath,
		Type:             "sendreceive",
		Devices:          []FolderDevice{{DeviceID: pairedDeviceID}},
		FSWatcherEnabled: true,
		MaxConflicts:     10,
	}
}

// NewHardenedOptions returns deterministic offline-oriented global settings.
func NewHardenedOptions() HardenedOptions {
	return HardenedOptions{URAccepted: -1}
}
