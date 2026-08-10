package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/credentials"
	"github.com/Dmitbd/remote-docker/internal/sshtransport"
	"github.com/Dmitbd/remote-docker/internal/syncer"
	"github.com/Dmitbd/remote-docker/internal/workspace"
)

func TestProductionSyncReadinessConfiguresScansAndWaitsForBothPeers(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-API-Key") != "local-api-key" {
			http.Error(response, "missing API key", http.StatusUnauthorized)
			return
		}
		value := request.Method + " " + request.URL.Path
		if request.URL.RawQuery != "" {
			value += "?" + request.URL.RawQuery
		}
		requests = append(requests, value)
		switch request.URL.Path {
		case "/rest/db/status":
			_, _ = response.Write([]byte(`{"state":"idle","needTotalItems":0,"pullErrors":0}`))
		case "/rest/system/connections":
			_, _ = response.Write([]byte(`{"connections":{"WINDOWS-SYNC":{"connected":true}}}`))
		default:
			response.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		ActiveDevice:  "pc-1", LocalSyncthingDeviceID: "MAC-SYNC", LocalSyncthingIdentity: []byte("sealed"),
		Devices: map[string]config.Device{
			"pc-1": {Name: "Windows PC", Address: "192.168.1.20", SyncPort: 49220, SyncthingDeviceID: "WINDOWS-SYNC"},
		},
		Workspaces: map[string]config.Workspace{
			"0123456789abcdef": {Path: "/Users/demo/project"},
			"fedcba9876543210": {Path: "/Users/demo/other"},
		},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	secrets := credentials.NewMemoryStore()
	if err := secrets.Put(localSyncthingCredentialOwner, syncer.SyncthingAPIKeyCredential, []byte("local-api-key")); err != nil {
		t.Fatalf("store local API key: %v", err)
	}
	remote := &recordingRemoteSyncOperations{}
	readiness := productionSyncReadiness{
		store: store, secrets: secrets, httpClient: server.Client(), endpoint: server.URL,
		remote: remote,
	}
	resolved := workspace.ResolvedPath{
		Local: "/Users/demo/project", Remote: "/Users/demo/project", WorkspaceID: "0123456789abcdef",
	}
	if err := readiness.EnsureFolder(context.Background(), resolved); err != nil {
		t.Fatalf("EnsureFolder() error = %v", err)
	}
	if err := readiness.Scan(context.Background(), resolved); err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if err := readiness.WaitBoth(context.Background(), resolved); err != nil {
		t.Fatalf("WaitBoth() error = %v", err)
	}
	if remote.deviceID != "MAC-SYNC" || len(remote.folders) != 2 || remote.folders[0].ID != "0123456789abcdef" ||
		remote.scanned != resolved.WorkspaceID || remote.statusFolder != resolved.WorkspaceID || remote.statusDevice != "MAC-SYNC" {
		t.Fatalf("remote operations = %#v", remote)
	}
	wantRequests := []string{
		"GET /rest/system/connections",
		"PUT /rest/config/devices", "PUT /rest/config/folders",
		"POST /rest/db/ignores?folder=0123456789abcdef", "POST /rest/db/ignores?folder=fedcba9876543210",
		"POST /rest/db/scan?folder=0123456789abcdef",
		"GET /rest/db/status?folder=0123456789abcdef", "GET /rest/system/connections",
	}
	if !reflect.DeepEqual(requests, wantRequests) {
		t.Fatalf("local requests = %#v, want %#v", requests, wantRequests)
	}
}

func TestSSHRemoteSyncUsesOnlyPinnedTypedRPC(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion, ActiveDevice: "pc-1",
		Devices: map[string]config.Device{"pc-1": {Address: "192.168.1.20"}},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	var methods []string
	client := sshRemoteSync{
		store: store, sshConfigPath: filepath.Join(root, "ssh_config"), sshBinary: "managed-ssh",
		run: func(_ context.Context, command sshtransport.Command) error {
			if command.Binary != "managed-ssh" || !reflect.DeepEqual(command.Args, []string{
				"-F", filepath.Join(root, "ssh_config"), "remote-docker-device-pc-1-control", "remote-docker-remote", "rpc",
			}) {
				t.Fatalf("SSH command = %#v", command)
			}
			var incoming struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      int             `json:"id"`
				Method  string          `json:"method"`
				Params  json.RawMessage `json:"params"`
			}
			if err := json.NewDecoder(command.Stdin).Decode(&incoming); err != nil {
				t.Fatalf("decode RPC input: %v", err)
			}
			methods = append(methods, incoming.Method)
			result := map[string]any{}
			switch incoming.Method {
			case "sync.configure":
				result["configured"] = true
			case "sync.scan":
				result["scanned"] = true
			case "sync.status":
				result = map[string]any{"state": "idle", "need_total_items": 0, "pull_errors": 0, "connected": true}
			default:
				t.Fatalf("unexpected RPC method %q", incoming.Method)
			}
			return json.NewEncoder(command.Stdout).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
		},
	}
	folders := []remoteWorkspaceFolder{{ID: "0123456789abcdef", Path: "/Users/demo/project"}}
	if err := client.Configure(context.Background(), "MAC-SYNC", folders); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	if err := client.Scan(context.Background(), folders[0].ID); err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	status, err := client.Status(context.Background(), folders[0].ID, "MAC-SYNC")
	if err != nil || status.State != "idle" || !status.Connected {
		t.Fatalf("Status() = %#v, %v", status, err)
	}
	if want := []string{"sync.configure", "sync.scan", "sync.status"}; !reflect.DeepEqual(methods, want) {
		t.Fatalf("methods = %#v, want %#v", methods, want)
	}
	if strings.Contains(strings.Join(methods, " "), "exec") || bytes.Contains([]byte(strings.Join(methods, " ")), []byte("shell")) {
		t.Fatalf("unsafe RPC methods = %#v", methods)
	}
}

type recordingRemoteSyncOperations struct {
	deviceID     string
	folders      []remoteWorkspaceFolder
	scanned      string
	statusFolder string
	statusDevice string
}

func (r *recordingRemoteSyncOperations) Configure(_ context.Context, deviceID string, folders []remoteWorkspaceFolder) error {
	r.deviceID = deviceID
	r.folders = append([]remoteWorkspaceFolder(nil), folders...)
	return nil
}

func (r *recordingRemoteSyncOperations) Scan(_ context.Context, folderID string) error {
	r.scanned = folderID
	return nil
}

func (r *recordingRemoteSyncOperations) Status(_ context.Context, folderID, deviceID string) (remoteWorkspaceStatus, error) {
	r.statusFolder = folderID
	r.statusDevice = deviceID
	return remoteWorkspaceStatus{State: "idle", Connected: true}, nil
}
