package syncer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/credentials"
)

func TestClientUsesAPIKeyAndExactSyncthingEndpoints(t *testing.T) {
	const apiKey = "test-api-key"
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("X-API-Key"); got != apiKey {
			t.Errorf("X-API-Key = %q", got)
		}
		requests = append(requests, request.Method+" "+request.URL.Path)
		switch request.URL.Path {
		case "/rest/db/status":
			_ = json.NewEncoder(response).Encode(FolderStatus{State: "idle"})
		case "/rest/system/connections":
			_ = json.NewEncoder(response).Encode(ConnectionsResponse{Connections: map[string]ConnectionStatus{
				"REMOTE": {Connected: true},
			}})
		default:
			response.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()

	secrets := credentials.NewMemoryStore()
	if err := secrets.Put("paired-device", SyncthingAPIKeyCredential, []byte(apiKey)); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.URL, "paired-device", secrets, server.Client())
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := client.ReplaceDevices(ctx, []DeviceConfig{{DeviceID: "REMOTE"}}); err != nil {
		t.Fatal(err)
	}
	if err := client.ReplaceFolders(ctx, []FolderConfig{{ID: "folder"}}); err != nil {
		t.Fatal(err)
	}
	if err := client.SetIgnores(ctx, "folder", []string{"(?d).git"}); err != nil {
		t.Fatal(err)
	}
	if err := client.Scan(ctx, "folder"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.FolderStatus(ctx, "folder"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Connections(ctx); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"PUT /rest/config/devices",
		"PUT /rest/config/folders",
		"POST /rest/db/ignores",
		"POST /rest/db/scan",
		"GET /rest/db/status",
		"GET /rest/system/connections",
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
}

func TestClientRequiresLoopbackRESTEndpoint(t *testing.T) {
	secrets := credentials.NewMemoryStore()
	for _, endpoint := range []string{
		"http://192.168.1.10:8384",
		"http://0.0.0.0:8384",
		"http://example.com:8384",
	} {
		if _, err := NewClient(endpoint, "device", secrets, http.DefaultClient); err == nil {
			t.Fatalf("NewClient(%q) succeeded", endpoint)
		}
	}
}

func TestClientDoesNotFollowRedirectsWithAPIKey(t *testing.T) {
	received := 0
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		received++
	}))
	defer destination.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	secrets := credentials.NewMemoryStore()
	if err := secrets.Put("device", SyncthingAPIKeyCredential, []byte("do-not-forward")); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(redirector.URL, "device", secrets, redirector.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ReplaceDevices(context.Background(), nil); err == nil {
		t.Fatal("redirect response was accepted")
	}
	if received != 0 {
		t.Fatal("API request followed a redirect")
	}
}

func TestHardenedSyncthingConfig(t *testing.T) {
	device, err := NewDeviceConfig("REMOTE", "Windows PC")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"tcp://127.0.0.1:49220"}; !reflect.DeepEqual(device.Addresses, want) {
		t.Fatalf("Addresses = %#v, want %#v", device.Addresses, want)
	}
	if device.AutoAcceptFolders || device.Introducer {
		t.Fatalf("unsafe device config = %#v", device)
	}
	if _, err := NewDeviceConfig("", "Windows"); err == nil {
		t.Fatal("empty device ID accepted")
	}
	passive, err := NewPassiveDeviceConfig("MAC", "Paired Mac")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"dynamic"}; !reflect.DeepEqual(passive.Addresses, want) || passive.AutoAcceptFolders || passive.Introducer {
		t.Fatalf("passive device config = %#v", passive)
	}

	folder := NewFolderConfig("workspace-1", "/Users/demo/projects/sample", "REMOTE")
	if folder.Type != "sendreceive" || folder.MaxConflicts != 10 || !folder.FSWatcherEnabled {
		t.Fatalf("folder defaults = %#v", folder)
	}
	if contains(DefaultIgnores, ".env") || contains(DefaultIgnores, "(?d).env") {
		t.Fatal(".env was ignored automatically")
	}
	wantIgnores := []string{
		"(?d).git", "(?d)node_modules", "(?d).pnpm-store", "(?d).turbo",
		"(?d)__pycache__", "(?d).pytest_cache", "(?d).mypy_cache",
		"(?d).idea", "(?d).vscode", "(?d)dist", "(?d)build",
	}
	if !reflect.DeepEqual(DefaultIgnores, wantIgnores) {
		t.Fatalf("DefaultIgnores = %#v", DefaultIgnores)
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
