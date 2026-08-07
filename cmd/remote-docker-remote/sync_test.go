package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultRemoteSyncRuntimeBoundsLoopbackRequests(t *testing.T) {
	runtime, ok := defaultRemoteSyncRuntime().(remoteSyncthingOperations)
	if !ok {
		t.Fatalf("default sync runtime type = %T", defaultRemoteSyncRuntime())
	}
	if runtime.Endpoint != "http://127.0.0.1:8384" || runtime.HTTPClient == nil ||
		runtime.HTTPClient.Timeout <= 0 || runtime.HTTPClient.Timeout > 30*time.Second {
		t.Fatalf("default remote Syncthing runtime = %#v", runtime)
	}
}

func TestRemoteSyncthingOperationsUseLoopbackAPIAndPairedIdentityOnly(t *testing.T) {
	root := t.TempDir()
	authorizedKeysPath := filepath.Join(root, "authorized_keys")
	if err := os.WriteFile(authorizedKeysPath, []byte(testAuthorizedLine(t, "MAC-SYNC")), 0o600); err != nil {
		t.Fatalf("write managed authorization: %v", err)
	}
	configPath := filepath.Join(root, "config.xml")
	if err := os.WriteFile(configPath, []byte(`<configuration><gui><apikey>remote-api-key</apikey></gui></configuration>`), 0o600); err != nil {
		t.Fatalf("write Syncthing config: %v", err)
	}

	type observedRequest struct {
		method string
		path   string
		query  string
		body   []byte
	}
	var requests []observedRequest
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-API-Key") != "remote-api-key" {
			http.Error(response, "missing API key", http.StatusUnauthorized)
			return
		}
		observed := observedRequest{method: request.Method, path: request.URL.Path, query: request.URL.RawQuery}
		if request.Body != nil && request.Header.Get("Content-Type") == "application/json" {
			observed.body, _ = io.ReadAll(request.Body)
		}
		requests = append(requests, observed)
		switch request.URL.Path {
		case "/rest/db/status":
			_, _ = response.Write([]byte(`{"state":"idle","needTotalItems":0,"pullErrors":0}`))
		case "/rest/system/connections":
			_, _ = response.Write([]byte(`{"connections":{"MAC-SYNC":{"connected":true}}}`))
		default:
			response.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()

	operations := remoteSyncthingOperations{
		Endpoint: server.URL, ConfigPath: configPath, AuthorizedKeysPath: authorizedKeysPath,
		HTTPClient: server.Client(),
	}
	params := remoteSyncConfigureParams{
		DeviceID: "MAC-SYNC",
		Folders:  []remoteSyncFolder{{ID: "0123456789abcdef", Path: "/Users/demo/project"}},
	}
	if err := operations.Configure(context.Background(), params); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	if err := operations.Scan(context.Background(), "0123456789abcdef"); err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	status, err := operations.Status(context.Background(), "0123456789abcdef", "MAC-SYNC")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.State != "idle" || status.NeedTotalItems != 0 || status.PullErrors != 0 || !status.Connected {
		t.Fatalf("Status() = %#v", status)
	}
	if err := operations.Revoke(context.Background(), "MAC-SYNC"); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	wantRequests := []string{
		"PUT /rest/config/devices", "PUT /rest/config/folders", "POST /rest/db/ignores?folder=0123456789abcdef",
		"POST /rest/db/scan?folder=0123456789abcdef",
		"GET /rest/db/status?folder=0123456789abcdef", "GET /rest/system/connections",
		"PUT /rest/config/folders", "PUT /rest/config/devices",
	}
	gotRequests := make([]string, 0, len(requests))
	for _, request := range requests {
		value := request.method + " " + request.path
		if request.query != "" {
			value += "?" + request.query
		}
		gotRequests = append(gotRequests, value)
	}
	if !reflect.DeepEqual(gotRequests, wantRequests) {
		t.Fatalf("requests = %#v, want %#v", gotRequests, wantRequests)
	}
	var bodies bytes.Buffer
	for _, request := range requests {
		bodies.Write(request.body)
	}
	if !bytes.Contains(bodies.Bytes(), []byte(`"addresses":["dynamic"]`)) ||
		!bytes.Contains(bodies.Bytes(), []byte(`"path":"/Users/demo/project"`)) ||
		!bytes.Contains(bodies.Bytes(), []byte(`"ignore":["(?d).git"`)) {
		t.Fatalf("managed request bodies = %s", bodies.Bytes())
	}
}

func TestRemoteSyncthingOperationsRejectForeignDeviceAndUnsafeFolder(t *testing.T) {
	root := t.TempDir()
	authorizedKeysPath := filepath.Join(root, "authorized_keys")
	if err := os.WriteFile(authorizedKeysPath, []byte(testAuthorizedLine(t, "MAC-SYNC")), 0o600); err != nil {
		t.Fatalf("write managed authorization: %v", err)
	}
	operations := remoteSyncthingOperations{AuthorizedKeysPath: authorizedKeysPath}
	tests := []remoteSyncConfigureParams{
		{DeviceID: "OTHER", Folders: []remoteSyncFolder{{ID: "0123456789abcdef", Path: "/Users/demo/project"}}},
		{DeviceID: "MAC-SYNC", Folders: []remoteSyncFolder{{ID: "0123456789abcdef", Path: "/etc"}}},
		{DeviceID: "MAC-SYNC", Folders: []remoteSyncFolder{{ID: "../escape", Path: "/Users/demo/project"}}},
	}
	for _, params := range tests {
		if err := operations.Configure(context.Background(), params); err == nil {
			t.Fatalf("Configure(%#v) succeeded", params)
		} else if strings.Contains(err.Error(), params.DeviceID) || strings.Contains(err.Error(), params.Folders[0].Path) {
			t.Fatalf("validation error leaked request data: %v", err)
		}
	}
}

func TestSyncBootstrapHardensOnlyExactRegularConfig(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.xml")
	input := []byte(`<configuration>
  <gui><address>0.0.0.0:8384</address><apikey>wsl-api-key</apikey></gui>
  <options>
    <listenAddress>default</listenAddress>
    <globalAnnounceEnabled>true</globalAnnounceEnabled><localAnnounceEnabled>true</localAnnounceEnabled>
    <relaysEnabled>true</relaysEnabled><startBrowser>true</startBrowser><urAccepted>0</urAccepted>
    <upgradeToPreReleases>true</upgradeToPreReleases>
  </options>
</configuration>`)
	if err := os.WriteFile(configPath, input, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if code := runSyncBootstrap(syncBootstrapRuntime{ConfigPath: configPath}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("runSyncBootstrap() code = %d", code)
	}
	hardened, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read hardened config: %v", err)
	}
	for _, required := range []string{
		"<address>127.0.0.1:8384</address>", "<apikey>wsl-api-key</apikey>",
		"<listenAddress>tcp://0.0.0.0:22000</listenAddress>", "<relaysEnabled>false</relaysEnabled>",
	} {
		if !bytes.Contains(hardened, []byte(required)) {
			t.Fatalf("hardened config missing %q: %s", required, hardened)
		}
	}

	target := filepath.Join(root, "foreign.xml")
	if err := os.WriteFile(target, input, 0o600); err != nil {
		t.Fatalf("write foreign config: %v", err)
	}
	link := filepath.Join(root, "config-link.xml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create config symlink: %v", err)
	}
	if code := runSyncBootstrap(syncBootstrapRuntime{ConfigPath: link}, &bytes.Buffer{}); code == 0 {
		t.Fatal("runSyncBootstrap() accepted a symlink")
	}
	if after, _ := os.ReadFile(target); !bytes.Equal(after, input) {
		t.Fatal("rejected symlink changed its target")
	}
}
