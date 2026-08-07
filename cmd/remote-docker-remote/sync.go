package main

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dmitbd/remote-docker/internal/credentials"
	"github.com/Dmitbd/remote-docker/internal/syncer"
)

const remoteSyncthingCredentialOwner = "remote-syncthing"

type remoteSyncFolder struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type remoteSyncConfigureParams struct {
	DeviceID string             `json:"device_id"`
	Folders  []remoteSyncFolder `json:"folders"`
}

type remoteSyncFolderParams struct {
	FolderID string `json:"folder_id"`
}

type remoteSyncStatusParams struct {
	FolderID string `json:"folder_id"`
	DeviceID string `json:"device_id"`
}

type remoteSyncStatus struct {
	State          string
	NeedTotalItems int
	PullErrors     int
	Connected      bool
}

type remoteSyncRuntime interface {
	Configure(context.Context, remoteSyncConfigureParams) error
	Scan(context.Context, string) error
	Status(context.Context, string, string) (remoteSyncStatus, error)
	Revoke(context.Context, string) error
}

func (o remoteSyncthingOperations) Revoke(ctx context.Context, deviceID string) error {
	if err := o.validateManagedDevice(deviceID); err != nil {
		return err
	}
	client, err := o.client()
	if err != nil {
		return err
	}
	if err := client.ReplaceFolders(ctx, nil); err != nil {
		return errors.New("remove managed sync folders")
	}
	if err := client.ReplaceDevices(ctx, nil); err != nil {
		return errors.New("remove managed sync device")
	}
	return o.persistSecretFreeConfig()
}

type remoteSyncthingOperations struct {
	Endpoint             string
	ConfigPath           string
	PersistentConfigPath string
	AuthorizedKeysPath   string
	HTTPClient           *http.Client
}

func defaultRemoteSyncRuntime() remoteSyncRuntime {
	return remoteSyncthingOperations{
		Endpoint: "http://127.0.0.1:8384", ConfigPath: "/run/remote-docker/syncthing/config.xml",
		PersistentConfigPath: "/var/lib/remote-docker/syncthing/config.xml",
		AuthorizedKeysPath:   "/var/lib/remote-docker/authorized_keys",
		HTTPClient:           &http.Client{Timeout: 10 * time.Second},
	}
}

func (o remoteSyncthingOperations) Configure(ctx context.Context, params remoteSyncConfigureParams) error {
	if err := o.validateManagedDevice(params.DeviceID); err != nil {
		return err
	}
	if len(params.Folders) > 64 {
		return errors.New("too many managed sync folders")
	}
	seenIDs := make(map[string]bool, len(params.Folders))
	seenPaths := make(map[string]bool, len(params.Folders))
	configuredFolders := make([]syncer.FolderConfig, 0, len(params.Folders))
	for _, folder := range params.Folders {
		if !validRemoteSyncFolder(folder) || seenIDs[folder.ID] || seenPaths[folder.Path] {
			return errors.New("managed sync folder is invalid")
		}
		seenIDs[folder.ID] = true
		seenPaths[folder.Path] = true
		configuredFolders = append(configuredFolders, syncer.NewFolderConfig(folder.ID, folder.Path, params.DeviceID))
	}
	device, err := syncer.NewPassiveDeviceConfig(params.DeviceID, "Paired Mac")
	if err != nil {
		return errors.New("managed sync device is invalid")
	}
	client, err := o.client()
	if err != nil {
		return err
	}
	if err := client.ReplaceDevices(ctx, []syncer.DeviceConfig{device}); err != nil {
		return errors.New("configure managed sync device")
	}
	if err := client.ReplaceFolders(ctx, configuredFolders); err != nil {
		return errors.New("configure managed sync folders")
	}
	for _, folder := range params.Folders {
		if err := client.SetIgnores(ctx, folder.ID, syncer.DefaultIgnores); err != nil {
			return errors.New("configure managed sync ignores")
		}
	}
	return o.persistSecretFreeConfig()
}

func (o remoteSyncthingOperations) persistSecretFreeConfig() error {
	runtimePath := o.ConfigPath
	if runtimePath == "" {
		runtimePath = "/run/remote-docker/syncthing/config.xml"
	}
	persistentPath := o.PersistentConfigPath
	if persistentPath == "" {
		persistentPath = "/var/lib/remote-docker/syncthing/config.xml"
	}
	contents, err := readRuntimeFile(runtimePath)
	if err != nil {
		return errors.New("read managed Syncthing runtime config")
	}
	sanitized, err := syncer.SanitizeConfigAPIKey(contents)
	if err != nil {
		return errors.New("sanitize managed Syncthing runtime config")
	}
	if err := writeRuntimeFile(persistentPath, sanitized, 0o600, -1, -1); err != nil {
		return errors.New("persist managed Syncthing config")
	}
	return nil
}

func (o remoteSyncthingOperations) Scan(ctx context.Context, folderID string) error {
	if !validRemoteSyncFolderID(folderID) {
		return errors.New("managed sync folder is invalid")
	}
	client, err := o.client()
	if err != nil {
		return err
	}
	if err := client.Scan(ctx, folderID); err != nil {
		return errors.New("scan managed sync folder")
	}
	return nil
}

func (o remoteSyncthingOperations) Status(ctx context.Context, folderID, deviceID string) (remoteSyncStatus, error) {
	if !validRemoteSyncFolderID(folderID) {
		return remoteSyncStatus{}, errors.New("managed sync folder is invalid")
	}
	if err := o.validateManagedDevice(deviceID); err != nil {
		return remoteSyncStatus{}, err
	}
	client, err := o.client()
	if err != nil {
		return remoteSyncStatus{}, err
	}
	folder, err := client.FolderStatus(ctx, folderID)
	if err != nil {
		return remoteSyncStatus{}, errors.New("read managed sync folder status")
	}
	connections, err := client.Connections(ctx)
	if err != nil {
		return remoteSyncStatus{}, errors.New("read managed sync connection status")
	}
	return remoteSyncStatus{
		State: folder.State, NeedTotalItems: folder.NeedTotalItems,
		PullErrors: folder.PullErrors, Connected: connections[deviceID].Connected,
	}, nil
}

func (o remoteSyncthingOperations) client() (*syncer.Client, error) {
	endpoint := o.Endpoint
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8384"
	}
	configPath := o.ConfigPath
	if configPath == "" {
		configPath = "/run/remote-docker/syncthing/config.xml"
	}
	httpClient := o.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return syncer.NewClient(endpoint, remoteSyncthingCredentialOwner, configAPIKeyStore{path: configPath}, httpClient)
}

func (o remoteSyncthingOperations) validateManagedDevice(deviceID string) error {
	path := o.AuthorizedKeysPath
	if path == "" {
		path = "/var/lib/remote-docker/authorized_keys"
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return errors.New("managed pairing identity is unavailable")
	}
	paired, ok := managedPairingDeviceID(contents)
	if !ok || paired != deviceID {
		return errors.New("managed sync device does not match pairing")
	}
	return nil
}

func validRemoteSyncFolder(folder remoteSyncFolder) bool {
	if !validRemoteSyncFolderID(folder.ID) || len(folder.Path) == 0 || len(folder.Path) > 4096 ||
		!filepath.IsAbs(folder.Path) || filepath.Clean(folder.Path) != folder.Path ||
		!strings.HasPrefix(folder.Path, "/Users/") || strings.ContainsRune(folder.Path, 0) {
		return false
	}
	return true
}

func validRemoteSyncFolderID(value string) bool {
	if len(value) != 16 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

type configAPIKeyStore struct{ path string }

func (s configAPIKeyStore) Get(deviceID, name string) ([]byte, error) {
	if deviceID != remoteSyncthingCredentialOwner || name != syncer.SyncthingAPIKeyCredential {
		return nil, credentials.ErrNotFound
	}
	info, err := os.Lstat(s.path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("managed Syncthing config is unavailable")
	}
	file, err := os.Open(s.path)
	if err != nil {
		return nil, errors.New("managed Syncthing config is unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return nil, errors.New("managed Syncthing config is invalid")
	}
	var parsed struct {
		GUI struct {
			APIKey string `xml:"apikey"`
		} `xml:"gui"`
	}
	if err := xml.Unmarshal(data, &parsed); err != nil || strings.TrimSpace(parsed.GUI.APIKey) == "" || len(parsed.GUI.APIKey) > 4096 {
		return nil, errors.New("managed Syncthing API credential is unavailable")
	}
	return []byte(strings.TrimSpace(parsed.GUI.APIKey)), nil
}

func (configAPIKeyStore) Put(string, string, []byte) error {
	return errors.New("managed Syncthing credential store is read-only")
}

func (configAPIKeyStore) Delete(string, string) error {
	return errors.New("managed Syncthing credential store is read-only")
}

var _ credentials.Store = configAPIKeyStore{}
