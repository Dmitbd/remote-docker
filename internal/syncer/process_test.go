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
		"--no-browser", "--no-restart", "--no-upgrade",
		"--gui-address=127.0.0.1:8384", "--data=" + dataDir,
		"--config=" + process.RuntimeDir(),
	} {
		if !contains(launcher.args, wanted) {
			t.Fatalf("Syncthing args %#v do not contain %q", launcher.args, wanted)
		}
	}
	if contains(launcher.args, "--no-default-folder") {
		t.Fatalf("Syncthing args contain removed v2 flag: %#v", launcher.args)
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

func TestBootstrapIdentityEncryptsKeysStoresAPISecretAndHardensPersistentConfig(t *testing.T) {
	root := t.TempDir()
	secrets := credentials.NewMemoryStore()
	result, err := BootstrapIdentity(context.Background(), BootstrapOptions{
		Executable: "/bundle/syncthing", PersistentConfigDir: filepath.Join(root, "config"),
		RuntimeRoot: filepath.Join(root, "runtime"), CredentialOwner: "local-syncthing",
		Secrets: secrets, Random: bytes.NewReader(bytes.Repeat([]byte{7}, 64)),
		Generator: fakeIdentityGenerator{},
	})
	if err != nil {
		t.Fatalf("BootstrapIdentity() error = %v", err)
	}
	if result.DeviceID != "MAC-SYNCTHING-ID" || len(result.EncryptedIdentity) == 0 {
		t.Fatalf("bootstrap result = %#v", result)
	}
	apiKey, err := secrets.Get("local-syncthing", SyncthingAPIKeyCredential)
	if err != nil || string(apiKey) != "local-api-secret" {
		t.Fatalf("stored API key = %q error=%v", apiKey, err)
	}
	identityKey, err := secrets.Get("local-syncthing", SyncthingIdentityKeyCredential)
	if err != nil || len(identityKey) != 32 {
		t.Fatalf("stored identity key length=%d error=%v", len(identityKey), err)
	}
	config, err := os.ReadFile(filepath.Join(root, "config", "config.xml"))
	if err != nil {
		t.Fatalf("read persistent config: %v", err)
	}
	for _, forbidden := range []string{"local-api-secret", ">true</globalAnnounceEnabled>", ">true</relaysEnabled>", ">default</listenAddress>"} {
		if strings.Contains(string(config), forbidden) {
			t.Fatalf("persistent config contains unsafe value %q: %s", forbidden, config)
		}
	}
	for _, required := range []string{"<address>127.0.0.1:8384</address>", "<globalAnnounceEnabled>false</globalAnnounceEnabled>", "<relaysEnabled>false</relaysEnabled>", "<listenAddress>tcp://127.0.0.1:22000</listenAddress>"} {
		if !strings.Contains(string(config), required) {
			t.Fatalf("persistent config missing %q: %s", required, config)
		}
	}
	for _, name := range []string{"cert.pem", "key.pem"} {
		if _, err := os.Stat(filepath.Join(root, "config", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("plaintext identity persisted as %s", name)
		}
	}
}

func TestHardenWSLConfigPreservesAPIKeyAndDisablesExternalDiscovery(t *testing.T) {
	input := []byte(`<configuration>
  <gui><address>0.0.0.0:8384</address><apikey>wsl-api-key</apikey></gui>
  <options>
    <listenAddress>default</listenAddress>
    <globalAnnounceEnabled>true</globalAnnounceEnabled>
    <localAnnounceEnabled>true</localAnnounceEnabled>
    <relaysEnabled>true</relaysEnabled>
    <startBrowser>true</startBrowser>
    <urAccepted>0</urAccepted>
    <upgradeToPreReleases>true</upgradeToPreReleases>
  </options>
</configuration>`)
	hardened, err := HardenWSLConfig(input)
	if err != nil {
		t.Fatalf("HardenWSLConfig() error = %v", err)
	}
	text := string(hardened)
	for _, required := range []string{
		"<address>127.0.0.1:8384</address>", "<apikey>wsl-api-key</apikey>",
		"<listenAddress>tcp://0.0.0.0:22000</listenAddress>",
		"<globalAnnounceEnabled>false</globalAnnounceEnabled>",
		"<localAnnounceEnabled>false</localAnnounceEnabled>",
		"<relaysEnabled>false</relaysEnabled>", "<urAccepted>-1</urAccepted>",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("hardened WSL config missing %q: %s", required, text)
		}
	}
}

type fakeIdentityGenerator struct{}

func (fakeIdentityGenerator) Generate(_ context.Context, _ string, home string) (string, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	files := map[string]string{
		"cert.pem":   "certificate",
		"key.pem":    "private-key",
		"config.xml": `<configuration version="1"><gui><address>0.0.0.0:8384</address><apikey>local-api-secret</apikey></gui><options><listenAddress>default</listenAddress><globalAnnounceEnabled>true</globalAnnounceEnabled><localAnnounceEnabled>true</localAnnounceEnabled><relaysEnabled>true</relaysEnabled><startBrowser>true</startBrowser><urAccepted>0</urAccepted><upgradeToPreReleases>true</upgradeToPreReleases></options></configuration>`,
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(home, name), []byte(contents), 0o600); err != nil {
			return "", err
		}
	}
	return "MAC-SYNCTHING-ID", nil
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

func TestManagedProcessKeepsSyncthingAPIKeyOutOfPersistentConfig(t *testing.T) {
	options := testProcessOptions(t)
	launcher := &capturingLauncher{}
	options.Launcher = launcher
	if err := os.MkdirAll(options.PersistentConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	persistent := []byte(`<configuration><gui><apikey></apikey></gui></configuration>`)
	if err := os.WriteFile(filepath.Join(options.PersistentConfigDir, "config.xml"), persistent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := options.Secrets.Put(options.DeviceID, SyncthingAPIKeyCredential, []byte("runtime-api-secret")); err != nil {
		t.Fatal(err)
	}

	process, err := StartManagedProcess(context.Background(), options)
	if err != nil {
		t.Fatalf("StartManagedProcess() error = %v", err)
	}
	runtimeConfig, err := os.ReadFile(filepath.Join(process.RuntimeDir(), "config.xml"))
	if err != nil || !strings.Contains(string(runtimeConfig), "runtime-api-secret") {
		t.Fatalf("runtime config did not materialize API key: %q error=%v", runtimeConfig, err)
	}
	stored, _ := os.ReadFile(filepath.Join(options.PersistentConfigDir, "config.xml"))
	if strings.Contains(string(stored), "runtime-api-secret") {
		t.Fatalf("persistent config exposed API key while running: %s", stored)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := process.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	stored, _ = os.ReadFile(filepath.Join(options.PersistentConfigDir, "config.xml"))
	if strings.Contains(string(stored), "runtime-api-secret") {
		t.Fatalf("persistent config exposed API key after stop: %s", stored)
	}
}

func TestManagedProcessOwnsChildBeyondStartupContext(t *testing.T) {
	options := testProcessOptions(t)
	launcher := &capturingLauncher{}
	options.Launcher = launcher
	startupCtx, cancelStartup := context.WithCancel(context.Background())
	process, err := StartManagedProcess(startupCtx, options)
	if err != nil {
		t.Fatalf("StartManagedProcess() error = %v", err)
	}
	cancelStartup()
	if err := launcher.context.Err(); err != nil {
		t.Fatalf("child context followed startup cancellation: %v", err)
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), time.Second)
	defer cancelStop()
	if err := process.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestSyncthingAPIKeyMaterializationRoundTripKeepsPersistentConfigSecretFree(t *testing.T) {
	original := []byte(`<configuration><gui><address>127.0.0.1:8384</address><apikey></apikey></gui></configuration>`)
	materialized, err := MaterializeConfigAPIKey(original, []byte("runtime-only-api-key"))
	if err != nil {
		t.Fatalf("MaterializeConfigAPIKey() error = %v", err)
	}
	if !bytes.Contains(materialized, []byte("runtime-only-api-key")) {
		t.Fatalf("materialized config = %q", materialized)
	}
	sanitized, err := SanitizeConfigAPIKey(materialized)
	if err != nil {
		t.Fatalf("SanitizeConfigAPIKey() error = %v", err)
	}
	if bytes.Contains(sanitized, []byte("runtime-only-api-key")) || !bytes.Contains(sanitized, []byte("<apikey></apikey>")) {
		t.Fatalf("sanitized config = %q", sanitized)
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
	context  context.Context
}

func (l *capturingLauncher) Start(ctx context.Context, _ string, args []string) (ChildProcess, error) {
	if l.startErr != nil {
		return nil, l.startErr
	}
	l.args = append([]string(nil), args...)
	l.context = ctx
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
