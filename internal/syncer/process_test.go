package syncer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/credentials"
)

func TestManagedProcessMaterializesAndCleansEncryptedIdentity(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "persistent-config")
	dataDir := filepath.Join(root, "data")
	runtimeRoot := filepath.Join(root, "runtime")
	secrets := credentials.NewMemoryStore()
	key := bytes.Repeat([]byte{7}, 32)
	if err := secrets.Put("paired-device", SyncthingIdentityKeyCredential, key); err != nil {
		t.Fatal(err)
	}
	blob, err := EncryptIdentity(Identity{
		CertificatePEM: []byte("test certificate"),
		PrivateKeyPEM:  []byte("test private key"),
	}, key, bytes.NewReader(bytes.Repeat([]byte{3}, 32)))
	if err != nil {
		t.Fatal(err)
	}

	launcher := &capturingLauncher{}
	process, err := StartManagedProcess(context.Background(), ProcessOptions{
		Executable:          "/bundled/syncthing",
		PersistentConfigDir: configDir,
		DataDir:             dataDir,
		RuntimeRoot:         runtimeRoot,
		GUIAddress:          "127.0.0.1:8384",
		DeviceID:            "paired-device",
		Secrets:             secrets,
		EncryptedIdentity:   blob,
		Launcher:            launcher,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"cert.pem", "key.pem"} {
		if _, err := os.Stat(filepath.Join(process.RuntimeDir(), name)); err != nil {
			t.Fatalf("runtime identity %s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(configDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("persistent config contains %s", name)
		}
	}
	for _, wanted := range []string{
		"--no-browser", "--no-restart", "--no-upgrade", "--no-default-folder",
		"--gui-address=127.0.0.1:8384", "--data=" + dataDir,
		"--config=" + process.RuntimeDir(),
	} {
		if !contains(launcher.args, wanted) {
			t.Fatalf("Syncthing args %#v do not contain %q", launcher.args, wanted)
		}
	}

	if err := process.MarkReady(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cert.pem", "key.pem"} {
		if _, err := os.Stat(filepath.Join(process.RuntimeDir(), name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("runtime identity %s survived readiness", name)
		}
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := process.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if launcher.child.signals != 1 || launcher.child.kills != 0 {
		t.Fatalf("child stop signals=%d kills=%d", launcher.child.signals, launcher.child.kills)
	}
	if _, err := os.Stat(process.RuntimeDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime directory survived stop: %v", err)
	}
}

func TestManagedProcessCrashAndRecoveryRemoveOnlyMarkedRuntimeCopies(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, managedRuntimePrefix+"stale")
	unmanaged := filepath.Join(root, "unmanaged")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, managedRuntimeMarker), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "key.pem"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(unmanaged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unmanaged, "key.pem"), []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := CleanupStaleRuntime(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marked stale runtime survived: %v", err)
	}
	if _, err := os.Stat(unmanaged); err != nil {
		t.Fatalf("unmanaged directory was removed: %v", err)
	}
}

func TestManagedProcessStartFailureCleansRuntimeIdentity(t *testing.T) {
	options := testProcessOptions(t)
	launcher := &capturingLauncher{startErr: errors.New("cannot start")}
	options.Launcher = launcher
	_, err := StartManagedProcess(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "start Syncthing") {
		t.Fatalf("StartManagedProcess() error = %v", err)
	}
	entries, readErr := os.ReadDir(options.RuntimeRoot)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("runtime identity survived start failure: %#v", entries)
	}
}

func testProcessOptions(t *testing.T) ProcessOptions {
	t.Helper()
	root := t.TempDir()
	secrets := credentials.NewMemoryStore()
	key := bytes.Repeat([]byte{9}, 32)
	if err := secrets.Put("device", SyncthingIdentityKeyCredential, key); err != nil {
		t.Fatal(err)
	}
	blob, err := EncryptIdentity(Identity{CertificatePEM: []byte("cert"), PrivateKeyPEM: []byte("key")}, key, bytes.NewReader(bytes.Repeat([]byte{2}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return ProcessOptions{
		Executable:          "/bundled/syncthing",
		PersistentConfigDir: filepath.Join(root, "config"),
		DataDir:             filepath.Join(root, "data"),
		RuntimeRoot:         filepath.Join(root, "runtime"),
		GUIAddress:          "127.0.0.1:8384",
		DeviceID:            "device",
		Secrets:             secrets,
		EncryptedIdentity:   blob,
	}
}

type capturingLauncher struct {
	args     []string
	child    *fakeChild
	startErr error
}

func (l *capturingLauncher) Start(_ context.Context, _ string, args []string) (ChildProcess, error) {
	if l.startErr != nil {
		return nil, l.startErr
	}
	l.args = append([]string(nil), args...)
	l.child = &fakeChild{done: make(chan error, 1)}
	return l.child, nil
}

type fakeChild struct {
	mu      sync.Mutex
	done    chan error
	signals int
	kills   int
}

func (c *fakeChild) Wait() error { return <-c.done }

func (c *fakeChild) Interrupt() error {
	c.mu.Lock()
	c.signals++
	c.mu.Unlock()
	c.done <- nil
	return nil
}

func (c *fakeChild) Kill() error {
	c.mu.Lock()
	c.kills++
	c.mu.Unlock()
	c.done <- errors.New("killed")
	return nil
}
