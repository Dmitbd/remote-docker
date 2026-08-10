package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/credentials"
	"github.com/Dmitbd/remote-docker/internal/sshtransport"
	"github.com/Dmitbd/remote-docker/internal/syncer"
	"github.com/Dmitbd/remote-docker/internal/workspace"
)

type remoteWorkspaceFolder struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type remoteWorkspaceStatus struct {
	State          string `json:"state"`
	NeedTotalItems int    `json:"need_total_items"`
	PullErrors     int    `json:"pull_errors"`
	Connected      bool   `json:"connected"`
}

type remoteSyncOperations interface {
	Configure(context.Context, string, []remoteWorkspaceFolder) error
	Scan(context.Context, string) error
	Status(context.Context, string, string) (remoteWorkspaceStatus, error)
}

type productionSyncReadiness struct {
	store      config.Store
	secrets    credentials.Store
	httpClient *http.Client
	endpoint   string
	remote     remoteSyncOperations
	interval   time.Duration
}

func (r productionSyncReadiness) EnsureFolder(ctx context.Context, requested workspace.ResolvedPath) error {
	client, cfg, device, folders, err := r.configuration(requested)
	if err != nil {
		return err
	}
	interval := r.interval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	if err := waitLocalSyncthingAPI(ctx, client, interval); err != nil {
		return errors.New("local Syncthing API is not ready")
	}
	remoteDevice, err := syncer.NewDeviceConfig(device.SyncthingDeviceID, device.Name)
	if err != nil {
		return errors.New("paired Syncthing bridge is invalid")
	}
	localFolders := make([]syncer.FolderConfig, 0, len(folders))
	for _, folder := range folders {
		localFolders = append(localFolders, syncer.NewFolderConfig(folder.ID, folder.Path, device.SyncthingDeviceID))
	}
	if err := client.ReplaceDevices(ctx, []syncer.DeviceConfig{remoteDevice}); err != nil {
		return errors.New("configure local Syncthing device")
	}
	if err := client.ReplaceFolders(ctx, localFolders); err != nil {
		return errors.New("configure local Syncthing folders")
	}
	for _, folder := range folders {
		if err := client.SetIgnores(ctx, folder.ID, syncer.DefaultIgnores); err != nil {
			return errors.New("configure local Syncthing ignores")
		}
	}
	if r.remote == nil {
		return errors.New("remote Syncthing configuration is unavailable")
	}
	if err := r.remote.Configure(ctx, cfg.LocalSyncthingDeviceID, folders); err != nil {
		return errors.New("configure remote Syncthing folders")
	}
	return nil
}

func (r productionSyncReadiness) Scan(ctx context.Context, requested workspace.ResolvedPath) error {
	client, _, _, _, err := r.configuration(requested)
	if err != nil {
		return err
	}
	if err := client.Scan(ctx, requested.WorkspaceID); err != nil {
		return errors.New("scan local Syncthing folder")
	}
	if r.remote == nil {
		return errors.New("remote Syncthing scan is unavailable")
	}
	if err := r.remote.Scan(ctx, requested.WorkspaceID); err != nil {
		return errors.New("scan remote Syncthing folder")
	}
	return nil
}

func (r productionSyncReadiness) WaitBoth(ctx context.Context, requested workspace.ResolvedPath) error {
	client, cfg, device, _, err := r.configuration(requested)
	if err != nil {
		return err
	}
	interval := r.interval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	if err := syncer.WaitReady(ctx, client, requested.WorkspaceID, device.SyncthingDeviceID, interval); err != nil {
		return errors.New("local Syncthing folder is not ready")
	}
	if r.remote == nil {
		return errors.New("remote Syncthing status is unavailable")
	}
	for {
		status, err := r.remote.Status(ctx, requested.WorkspaceID, cfg.LocalSyncthingDeviceID)
		if err != nil {
			return errors.New("read remote Syncthing status")
		}
		if status.State == "idle" && status.NeedTotalItems == 0 && status.PullErrors == 0 && status.Connected {
			return nil
		}
		if err := waitRuntimeInterval(ctx, interval); err != nil {
			return errors.New("remote Syncthing folder is not ready")
		}
	}
}

func (r productionSyncReadiness) configuration(
	requested workspace.ResolvedPath,
) (*syncer.Client, config.Config, config.Device, []remoteWorkspaceFolder, error) {
	cfg, err := loadAgentConfig(r.store)
	if err != nil || cfg.ActiveDevice == "" || strings.TrimSpace(cfg.LocalSyncthingDeviceID) == "" {
		return nil, config.Config{}, config.Device{}, nil, errors.New("Syncthing pairing is incomplete")
	}
	device, ok := cfg.Devices[cfg.ActiveDevice]
	if !ok || strings.TrimSpace(device.SyncthingDeviceID) == "" {
		return nil, config.Config{}, config.Device{}, nil, errors.New("paired Syncthing device is unavailable")
	}
	registered, ok := cfg.Workspaces[requested.WorkspaceID]
	if !ok || registered.Path != requested.Local || requested.Remote != requested.Local {
		return nil, config.Config{}, config.Device{}, nil, errors.New("managed workspace does not match registration")
	}
	ids := make([]string, 0, len(cfg.Workspaces))
	for id := range cfg.Workspaces {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	folders := make([]remoteWorkspaceFolder, 0, len(ids))
	for _, id := range ids {
		path := cfg.Workspaces[id].Path
		if !validMacSyncPath(path) {
			return nil, config.Config{}, config.Device{}, nil, errors.New("managed workspace path is unsupported")
		}
		folders = append(folders, remoteWorkspaceFolder{ID: id, Path: path})
	}
	endpoint := r.endpoint
	if endpoint == "" {
		endpoint = localSyncthingEndpoint
	}
	client, err := syncer.NewClient(endpoint, localSyncthingCredentialOwner, r.secrets, r.httpClient)
	if err != nil {
		return nil, config.Config{}, config.Device{}, nil, err
	}
	return client, cfg, device, folders, nil
}

func validMacSyncPath(value string) bool {
	return len(value) > len("/Users/") && len(value) <= 4096 && filepath.IsAbs(value) &&
		filepath.Clean(value) == value && strings.HasPrefix(value, "/Users/") && !strings.ContainsRune(value, 0)
}

type remoteSyncMethod string

const (
	remoteSyncConfigure remoteSyncMethod = "sync.configure"
	remoteSyncScan      remoteSyncMethod = "sync.scan"
	remoteSyncStatusRPC remoteSyncMethod = "sync.status"
)

type sshRemoteSync struct {
	store         config.Store
	sshConfigPath string
	sshBinary     string
	run           func(context.Context, sshtransport.Command) error
}

func (c sshRemoteSync) Configure(ctx context.Context, deviceID string, folders []remoteWorkspaceFolder) error {
	var result struct {
		Configured bool `json:"configured"`
	}
	params := struct {
		DeviceID string                  `json:"device_id"`
		Folders  []remoteWorkspaceFolder `json:"folders"`
	}{DeviceID: deviceID, Folders: folders}
	if err := c.call(ctx, remoteSyncConfigure, params, &result); err != nil || !result.Configured {
		return errors.New("remote sync configuration was not acknowledged")
	}
	return nil
}

func (c sshRemoteSync) Scan(ctx context.Context, folderID string) error {
	var result struct {
		Scanned bool `json:"scanned"`
	}
	params := struct {
		FolderID string `json:"folder_id"`
	}{FolderID: folderID}
	if err := c.call(ctx, remoteSyncScan, params, &result); err != nil || !result.Scanned {
		return errors.New("remote sync scan was not acknowledged")
	}
	return nil
}

func (c sshRemoteSync) Status(ctx context.Context, folderID, deviceID string) (remoteWorkspaceStatus, error) {
	params := struct {
		FolderID string `json:"folder_id"`
		DeviceID string `json:"device_id"`
	}{FolderID: folderID, DeviceID: deviceID}
	var result remoteWorkspaceStatus
	if err := c.call(ctx, remoteSyncStatusRPC, params, &result); err != nil {
		return remoteWorkspaceStatus{}, errors.New("remote sync status was not acknowledged")
	}
	return result, nil
}

func (c sshRemoteSync) call(ctx context.Context, method remoteSyncMethod, params, destination any) error {
	if method != remoteSyncConfigure && method != remoteSyncScan && method != remoteSyncStatusRPC {
		return errors.New("unsupported managed sync RPC")
	}
	cfg, err := loadAgentConfig(c.store)
	if err != nil || cfg.ActiveDevice == "" {
		return errors.New("managed sync device is unavailable")
	}
	if _, ok := cfg.Devices[cfg.ActiveDevice]; !ok {
		return errors.New("managed sync device is unavailable")
	}
	alias, err := sshtransport.ControlAlias(cfg.ActiveDevice)
	if err != nil {
		return errors.New("managed sync device is invalid")
	}
	request, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": string(method), "params": params,
	})
	if err != nil {
		return errors.New("encode managed sync RPC")
	}
	binary := c.sshBinary
	if binary == "" {
		binary = "ssh"
	}
	var output bytes.Buffer
	command := sshtransport.Command{
		Binary: binary,
		Args: []string{
			"-F", c.sshConfigPath, alias,
			"remote-docker-remote", "rpc",
		},
		Stdin: bytes.NewReader(append(request, '\n')), Stdout: &output, Stderr: io.Discard,
	}
	run := c.run
	if run == nil {
		run = runSSHCommand
	}
	if err := run(ctx, command); err != nil {
		return errors.New("managed sync RPC failed")
	}
	var response struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(&output, 64<<10)).Decode(&response); err != nil ||
		response.JSONRPC != "2.0" || response.ID != 1 || len(response.Error) != 0 || len(response.Result) == 0 {
		return errors.New("managed sync RPC was not acknowledged")
	}
	decoder := json.NewDecoder(bytes.NewReader(response.Result))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("managed sync RPC returned invalid result")
	}
	return nil
}

var _ SyncReadiness = productionSyncReadiness{}
var _ remoteSyncOperations = sshRemoteSync{}
