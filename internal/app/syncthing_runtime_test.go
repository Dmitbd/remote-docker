package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/credentials"
	"github.com/Dmitbd/remote-docker/internal/syncer"
)

func TestLocalSyncthingRuntimeBootstrapsAndPersistsIdentityOnce(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{SchemaVersion: config.CurrentSchemaVersion}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	secrets := credentials.NewMemoryStore()
	bootstrapCalls := 0
	runtime := newLocalSyncthingRuntime(localSyncthingOptions{
		Store: store, Secrets: secrets, Executable: "/managed/syncthing",
		PersistentConfigDir: filepath.Join(root, "persistent"),
		DataDir:             filepath.Join(root, "data"),
		RuntimeRoot:         filepath.Join(root, "runtime"),
		Bootstrap: func(_ context.Context, options syncer.BootstrapOptions) (syncer.BootstrapResult, error) {
			bootstrapCalls++
			if options.CredentialOwner != localSyncthingCredentialOwner || options.Secrets != secrets {
				t.Fatalf("bootstrap options = %#v", options)
			}
			return syncer.BootstrapResult{DeviceID: "LOCAL-SYNC", EncryptedIdentity: []byte("sealed")}, nil
		},
	})

	first, err := runtime.DeviceID(context.Background())
	if err != nil {
		t.Fatalf("DeviceID() error = %v", err)
	}
	second, err := runtime.DeviceID(context.Background())
	if err != nil {
		t.Fatalf("second DeviceID() error = %v", err)
	}
	if first != "LOCAL-SYNC" || second != first || bootstrapCalls != 1 {
		t.Fatalf("device IDs = %q/%q bootstrap calls = %d", first, second, bootstrapCalls)
	}
	persisted, err := store.Load()
	if err != nil {
		t.Fatalf("load persisted config: %v", err)
	}
	if persisted.LocalSyncthingDeviceID != "LOCAL-SYNC" || !reflect.DeepEqual(persisted.LocalSyncthingIdentity, []byte("sealed")) {
		t.Fatalf("persisted local Syncthing identity = %#v", persisted)
	}
}

func TestLocalSyncthingRuntimeOwnsReadyProcessUntilCancellation(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{
		SchemaVersion:          config.CurrentSchemaVersion,
		LocalSyncthingDeviceID: "LOCAL-SYNC",
		LocalSyncthingIdentity: []byte("sealed"),
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	secrets := credentials.NewMemoryStore()
	process := newFakeLocalSyncthingProcess()
	started := make(chan syncer.ProcessOptions, 1)
	runtime := newLocalSyncthingRuntime(localSyncthingOptions{
		Store: store, Secrets: secrets, Executable: "/managed/syncthing",
		PersistentConfigDir: filepath.Join(root, "persistent"),
		DataDir:             filepath.Join(root, "data"),
		RuntimeRoot:         filepath.Join(root, "runtime"),
		Start: func(_ context.Context, options syncer.ProcessOptions) (localSyncthingProcess, error) {
			started <- options
			return process, nil
		},
		WaitAPI: func(context.Context, *syncer.Client, time.Duration) error { return nil },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx, time.Millisecond) }()

	var options syncer.ProcessOptions
	select {
	case options = <-started:
	case <-time.After(time.Second):
		t.Fatal("managed Syncthing process was not started")
	}
	select {
	case <-process.ready:
	case <-time.After(time.Second):
		t.Fatal("managed Syncthing identity was not cleared after API readiness")
	}
	if options.DeviceID != localSyncthingCredentialOwner || options.Executable != "/managed/syncthing" ||
		!reflect.DeepEqual(options.EncryptedIdentity, []byte("sealed")) {
		t.Fatalf("process options = %#v", options)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("managed Syncthing runtime did not stop")
	}
	select {
	case <-process.stopped:
	default:
		t.Fatal("managed Syncthing child was not stopped")
	}
	if _, err := os.Stat(options.PersistentConfigDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect persistent config directory: %v", err)
	}
}

func TestProductionSyncInspectorUsesLocalCredentialNamespace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/rest/system/connections" || request.Header.Get("X-API-Key") != "local-api-key" {
			http.Error(response, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = response.Write([]byte(`{"connections":{"WINDOWS-SYNC":{"connected":true}}}`))
	}))
	defer server.Close()
	secrets := credentials.NewMemoryStore()
	if err := secrets.Put(localSyncthingCredentialOwner, syncer.SyncthingAPIKeyCredential, []byte("local-api-key")); err != nil {
		t.Fatalf("store local API key: %v", err)
	}
	inspector := productionSyncInspector{secrets: secrets, httpClient: server.Client(), endpoint: server.URL}
	connected, err := inspector.Connected(context.Background(), config.Config{
		ActiveDevice: "paired-host",
		Devices: map[string]config.Device{
			"paired-host": {SyncthingDeviceID: "WINDOWS-SYNC"},
		},
	})
	if err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	if !connected {
		t.Fatal("Connected() = false, want true")
	}
}

type fakeLocalSyncthingProcess struct {
	ready   chan struct{}
	stopped chan struct{}
	once    sync.Once
}

func newFakeLocalSyncthingProcess() *fakeLocalSyncthingProcess {
	return &fakeLocalSyncthingProcess{ready: make(chan struct{}), stopped: make(chan struct{})}
}

func (p *fakeLocalSyncthingProcess) MarkReady() error {
	select {
	case <-p.ready:
	default:
		close(p.ready)
	}
	return nil
}

func (p *fakeLocalSyncthingProcess) Stop(context.Context) error {
	p.once.Do(func() { close(p.stopped) })
	return nil
}

func (p *fakeLocalSyncthingProcess) Wait(ctx context.Context) error {
	select {
	case <-p.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
