package app

import (
	"bytes"
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

func TestLocalSyncthingPrepareResetsOnlyCorruptUnpairedIdentity(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	validKey := bytes.Repeat([]byte{4}, 32)
	wrongKey := bytes.Repeat([]byte{5}, 32)
	blob, err := syncer.EncryptIdentity(syncer.Identity{
		CertificatePEM: []byte("certificate"), PrivateKeyPEM: []byte("private-key"),
	}, validKey, bytes.NewReader(bytes.Repeat([]byte{6}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	wantWorkspaces := map[string]config.Workspace{"project": {Path: "/Users/test/project"}}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion, LocalSyncthingDeviceID: "OLD-SYNC",
		LocalSyncthingIdentity: blob, Workspaces: wantWorkspaces,
	}); err != nil {
		t.Fatal(err)
	}
	secrets := credentials.NewMemoryStore()
	for owner, secret := range map[string]struct {
		name  string
		value []byte
	}{
		localSyncthingCredentialOwner: {name: syncer.SyncthingIdentityKeyCredential, value: wrongKey},
		"unrelated":                   {name: "keep", value: []byte("unrelated-secret")},
	} {
		if err := secrets.Put(owner, secret.name, secret.value); err != nil {
			t.Fatal(err)
		}
	}
	if err := secrets.Put(localSyncthingCredentialOwner, syncer.SyncthingAPIKeyCredential, []byte("old-api-key")); err != nil {
		t.Fatal(err)
	}

	runtime := newLocalSyncthingRuntime(localSyncthingOptions{
		Store: store, Secrets: secrets, Executable: "/managed/syncthing",
	})
	result, err := runtime.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !result.Recovered || result.CredentialCleanupError != nil {
		t.Fatalf("Prepare() result = %#v", result)
	}
	persisted, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.LocalSyncthingDeviceID != "" || len(persisted.LocalSyncthingIdentity) != 0 {
		t.Fatalf("stored identity survived recovery: %#v", persisted)
	}
	if !reflect.DeepEqual(persisted.Workspaces, wantWorkspaces) {
		t.Fatalf("workspaces = %#v, want %#v", persisted.Workspaces, wantWorkspaces)
	}
	for _, name := range []string{syncer.SyncthingIdentityKeyCredential, syncer.SyncthingAPIKeyCredential} {
		if _, err := secrets.Get(localSyncthingCredentialOwner, name); !errors.Is(err, credentials.ErrNotFound) {
			t.Fatalf("local credential %q survived: %v", name, err)
		}
	}
	if value, err := secrets.Get("unrelated", "keep"); err != nil || string(value) != "unrelated-secret" {
		t.Fatalf("unrelated credential = %q error=%v", value, err)
	}
}

func TestLocalSyncthingPrepareTreatsMissingIdentityKeyAsRecoverableOnlyWhenUnpaired(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion, LocalSyncthingDeviceID: "OLD-SYNC",
		LocalSyncthingIdentity: []byte("sealed"),
	}); err != nil {
		t.Fatal(err)
	}
	runtime := newLocalSyncthingRuntime(localSyncthingOptions{
		Store: store, Secrets: credentials.NewMemoryStore(), Executable: "/managed/syncthing",
	})
	result, err := runtime.Prepare(context.Background())
	if err != nil || !result.Recovered {
		t.Fatalf("Prepare() result=%#v error=%v", result, err)
	}
	persisted, err := store.Load()
	if err != nil || persisted.LocalSyncthingDeviceID != "" || len(persisted.LocalSyncthingIdentity) != 0 {
		t.Fatalf("persisted config=%#v error=%v", persisted, err)
	}
}

func TestLocalSyncthingPrepareResetsIncompleteUnpairedIdentityState(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	key := bytes.Repeat([]byte{2}, 32)
	blob, err := syncer.EncryptIdentity(syncer.Identity{
		CertificatePEM: []byte("certificate"), PrivateKeyPEM: []byte("private-key"),
	}, key, bytes.NewReader(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion, LocalSyncthingIdentity: blob,
	}); err != nil {
		t.Fatal(err)
	}
	secrets := credentials.NewMemoryStore()
	if err := secrets.Put(localSyncthingCredentialOwner, syncer.SyncthingIdentityKeyCredential, key); err != nil {
		t.Fatal(err)
	}
	runtime := newLocalSyncthingRuntime(localSyncthingOptions{Store: store, Secrets: secrets, Executable: "/managed/syncthing"})
	result, err := runtime.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !result.Recovered {
		t.Fatalf("Prepare() result = %#v, want recovery", result)
	}
}

func TestLocalSyncthingPrepareDoesNotRotateAmbiguousDeviceState(t *testing.T) {
	for name, mutate := range map[string]func(*config.Config){
		"active device": func(cfg *config.Config) {
			cfg.ActiveDevice = "windows"
			cfg.Devices = map[string]config.Device{"windows": {Name: "Windows", Address: "192.168.1.20"}}
		},
		"dormant device": func(cfg *config.Config) {
			cfg.Devices = map[string]config.Device{"windows": {Name: "Windows", Address: "192.168.1.20"}}
		},
		"pending cleanup": func(cfg *config.Config) {
			cfg.PendingRevocations = map[string]config.PendingRevocation{"generation": {Generation: "generation"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			store := config.Store{Path: filepath.Join(root, "config.json")}
			cfg := config.Config{
				SchemaVersion: config.CurrentSchemaVersion, LocalSyncthingDeviceID: "OLD-SYNC",
				LocalSyncthingIdentity: []byte("sealed"),
			}
			mutate(&cfg)
			if err := store.Save(cfg); err != nil {
				t.Fatal(err)
			}
			secrets := credentials.NewMemoryStore()
			if err := secrets.Put(localSyncthingCredentialOwner, syncer.SyncthingIdentityKeyCredential, bytes.Repeat([]byte{9}, 32)); err != nil {
				t.Fatal(err)
			}
			runtime := newLocalSyncthingRuntime(localSyncthingOptions{
				Store: store, Secrets: secrets, Executable: "/managed/syncthing",
			})
			_, err := runtime.Prepare(context.Background())
			var blocked *localSyncIdentityBlockedError
			if !errors.As(err, &blocked) {
				t.Fatalf("Prepare() error = %v, want localSyncIdentityBlockedError", err)
			}
			persisted, loadErr := store.Load()
			if loadErr != nil || !reflect.DeepEqual(persisted, cfg) {
				t.Fatalf("config changed: got=%#v want=%#v error=%v", persisted, cfg, loadErr)
			}
			if _, getErr := secrets.Get(localSyncthingCredentialOwner, syncer.SyncthingIdentityKeyCredential); getErr != nil {
				t.Fatalf("identity credential was removed: %v", getErr)
			}
		})
	}
}

func TestLocalSyncthingPrepareKeepsConfigForNonRecoverableCredentialError(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	want := config.Config{
		SchemaVersion: config.CurrentSchemaVersion, LocalSyncthingDeviceID: "OLD-SYNC",
		LocalSyncthingIdentity: []byte("sealed"),
	}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("keychain access denied")
	secrets := &recordingCredentialStore{base: credentials.NewMemoryStore(), getErr: wantErr}
	runtime := newLocalSyncthingRuntime(localSyncthingOptions{Store: store, Secrets: secrets, Executable: "/managed/syncthing"})
	if _, err := runtime.Prepare(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Prepare() error = %v, want %v", err, wantErr)
	}
	persisted, err := store.Load()
	if err != nil || !reflect.DeepEqual(persisted, want) || len(secrets.deleted) != 0 {
		t.Fatalf("failed-closed config=%#v deletes=%v error=%v", persisted, secrets.deleted, err)
	}
}

func TestLocalSyncthingPrepareCommitsConfigBeforeCredentialDeletion(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion, LocalSyncthingDeviceID: "OLD-SYNC",
		LocalSyncthingIdentity: []byte("sealed"),
	}); err != nil {
		t.Fatal(err)
	}
	base := credentials.NewMemoryStore()
	if err := base.Put(localSyncthingCredentialOwner, syncer.SyncthingIdentityKeyCredential, bytes.Repeat([]byte{3}, 32)); err != nil {
		t.Fatal(err)
	}
	secrets := &recordingCredentialStore{base: base, beforeDelete: func() {
		cfg, err := store.Load()
		if err != nil || cfg.LocalSyncthingDeviceID != "" || len(cfg.LocalSyncthingIdentity) != 0 {
			t.Fatalf("credential deletion ran before config commit: %#v error=%v", cfg, err)
		}
	}}
	runtime := newLocalSyncthingRuntime(localSyncthingOptions{Store: store, Secrets: secrets, Executable: "/managed/syncthing"})
	if result, err := runtime.Prepare(context.Background()); err != nil || !result.Recovered {
		t.Fatalf("Prepare() result=%#v error=%v", result, err)
	}
}

func TestLocalSyncthingPrepareIsIdempotentAfterCredentialCleanupFailure(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion, LocalSyncthingDeviceID: "OLD-SYNC",
		LocalSyncthingIdentity: []byte("sealed"),
	}); err != nil {
		t.Fatal(err)
	}
	base := credentials.NewMemoryStore()
	if err := base.Put(localSyncthingCredentialOwner, syncer.SyncthingIdentityKeyCredential, bytes.Repeat([]byte{3}, 32)); err != nil {
		t.Fatal(err)
	}
	secrets := &recordingCredentialStore{base: base, deleteErr: errors.New("temporary delete failure")}
	runtime := newLocalSyncthingRuntime(localSyncthingOptions{Store: store, Secrets: secrets, Executable: "/managed/syncthing"})
	first, err := runtime.Prepare(context.Background())
	if err != nil || !first.Recovered || first.CredentialCleanupError == nil {
		t.Fatalf("first Prepare() result=%#v error=%v", first, err)
	}
	second, err := runtime.Prepare(context.Background())
	if err != nil || second.Recovered {
		t.Fatalf("second Prepare() result=%#v error=%v", second, err)
	}
}

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

type recordingCredentialStore struct {
	base         credentials.Store
	getErr       error
	deleteErr    error
	beforeDelete func()
	deleted      []string
}

func (s *recordingCredentialStore) Put(deviceID, name string, value []byte) error {
	return s.base.Put(deviceID, name, value)
}

func (s *recordingCredentialStore) Get(deviceID, name string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.base.Get(deviceID, name)
}

func (s *recordingCredentialStore) Delete(deviceID, name string) error {
	if s.beforeDelete != nil {
		s.beforeDelete()
	}
	s.deleted = append(s.deleted, deviceID+"/"+name)
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.base.Delete(deviceID, name)
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
