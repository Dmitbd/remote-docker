package app

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/credentials"
	"github.com/Dmitbd/remote-docker/internal/dockercli"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/pairing"
	"github.com/Dmitbd/remote-docker/internal/portrelay"
	"github.com/Dmitbd/remote-docker/internal/sshtransport"
	"golang.org/x/crypto/ssh"
)

func TestProductionAgentRuntimeServesPersistedStateOverLocalSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket integration is covered on Unix builders")
	}
	root, err := os.MkdirTemp("/private/tmp", "rd-agent-e2e-")
	if err != nil {
		t.Fatalf("create short test runtime root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("protect test runtime root: %v", err)
	}
	configPath := filepath.Join(root, "config.json")
	store := config.Store{Path: configPath}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		Devices: map[string]config.Device{
			"pc-1": {Name: "Dev PC", Address: "192.168.1.20", SSHPort: 2222},
		},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	runtimeAgent, err := NewProductionAgentRuntime(ProductionAgentOptions{
		ConfigPath:     configPath,
		ExecutablePath: filepath.Join(root, "bin", "remote-docker-agent"),
	})
	if err != nil {
		t.Fatalf("NewProductionAgentRuntime() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runDone := make(chan error, 1)
	go func() { runDone <- runtimeAgent.Run(ctx, 5*time.Millisecond) }()

	endpoint := filepath.Join(root, "agent.sock")
	listener, err := localapi.Listen(endpoint)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- (localapi.Server{Handler: runtimeAgent.Agent()}).Serve(ctx, listener) }()

	client := localapi.Client{Endpoint: endpoint}
	var devices localapi.ListDevicesResult
	if err := client.Call(ctx, localapi.MethodListDevices, nil, &devices); err != nil {
		t.Fatalf("ListDevices error = %v", err)
	}
	if len(devices.Devices) != 1 || devices.Devices[0].ID != "pc-1" || devices.Devices[0].Name != "Dev PC" {
		t.Fatalf("devices = %#v", devices.Devices)
	}

	workspacePath := filepath.Join(root, "sample")
	if err := os.Mkdir(workspacePath, 0o700); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	var added localapi.Workspace
	if err := client.Call(ctx, localapi.MethodWorkspaceAdd, localapi.WorkspaceAddParams{Path: workspacePath}, &added); err != nil {
		t.Fatalf("WorkspaceAdd error = %v", err)
	}
	if added.ID == "" || added.Path != workspacePath {
		t.Fatalf("added workspace = %#v", added)
	}
	var workspaces localapi.WorkspaceListResult
	if err := client.Call(ctx, localapi.MethodWorkspaceList, nil, &workspaces); err != nil {
		t.Fatalf("WorkspaceList error = %v", err)
	}
	if len(workspaces.Workspaces) != 1 || workspaces.Workspaces[0] != added {
		t.Fatalf("workspaces = %#v, want %#v", workspaces.Workspaces, added)
	}
	persisted, err := store.Load()
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if persisted.Workspaces[added.ID].Path != workspacePath {
		t.Fatalf("persisted workspaces = %#v", persisted.Workspaces)
	}

	var status localapi.StatusResult
	if err := client.Call(ctx, localapi.MethodStatus, nil, &status); err != nil {
		t.Fatalf("Status error = %v", err)
	}
	if status.State != string(AgentUnpaired) {
		t.Fatalf("status = %#v, want Unpaired", status)
	}
	var remote *localapi.RemoteError
	var recovered localapi.RecoverResult
	err = client.Call(ctx, localapi.MethodRecover, nil, &recovered)
	remote = nil
	if !errors.As(err, &remote) || remote.Code != localapi.ErrorNeedsAction {
		t.Fatalf("Recover error = %T %v, want needs_action", err, err)
	}

	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestMacPairingCoordinatorPersistsPinnedDeviceAndRevokesBeforeLocalRemoval(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	transport := &runtimePairingTransport{hostKey: testAuthorizedKey(t)}
	docker := &runtimeDockerExecutor{}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: docker,
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		SSHConfigPath:   filepath.Join(root, "ssh_config"),
		KnownHostsPath:  filepath.Join(root, "known_hosts"),
		AgentSocketPath: filepath.Join(root, "ssh-agent.sock"),
		ControlDir:      filepath.Join(root, "control"),
	})

	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if started.SessionID == "" || len(started.Code) != 6 {
		t.Fatalf("pair start = %#v", started)
	}
	confirmed, err := coordinator.Confirm(context.Background(), localapi.PairConfirmParams{
		SessionID: started.SessionID, Code: started.Code,
	})
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if confirmed.Device.ID == "" || confirmed.Device.Address != "192.168.1.20" {
		t.Fatalf("confirmed = %#v", confirmed)
	}
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	device := cfg.Devices[confirmed.Device.ID]
	if cfg.ActiveDevice != confirmed.Device.ID || device.SSHHostPublicKey != transport.hostKey ||
		device.SyncthingDeviceID != "WINDOWS-SYNC" || device.ClientDeviceID == "" ||
		device.SSHPort != 49222 || device.SyncPort != 49220 {
		t.Fatalf("persisted device = %#v config=%#v", device, cfg)
	}
	privateKey, err := secrets.Get(confirmed.Device.ID, sshtransport.SSHPrivateKeyCredential)
	if err != nil || len(privateKey) == 0 {
		t.Fatalf("stored private key length=%d error=%v", len(privateKey), err)
	}
	knownHosts, _ := os.ReadFile(filepath.Join(root, "known_hosts"))
	if !strings.Contains(string(knownHosts), transport.hostKey) {
		t.Fatalf("known_hosts = %q", knownHosts)
	}
	if len(docker.calls) != 2 {
		t.Fatalf("Docker context calls = %#v", docker.calls)
	}

	if err := coordinator.Unpair(context.Background(), confirmed.Device.ID); err != nil {
		t.Fatalf("Unpair() error = %v", err)
	}
	if transport.revoked != device.ClientDeviceID {
		t.Fatalf("remote revoked device = %q, want %q", transport.revoked, device.ClientDeviceID)
	}
	cfg, _ = store.Load()
	if cfg.ActiveDevice != "" || len(cfg.Devices) != 0 {
		t.Fatalf("config after unpair = %#v", cfg)
	}
	if _, err := secrets.Get(confirmed.Device.ID, sshtransport.SSHPrivateKeyCredential); !errors.Is(err, credentials.ErrNotFound) {
		t.Fatalf("private key after unpair error = %v", err)
	}
}

type runtimePairingTransport struct {
	hostKey string
	private ed25519.PrivateKey
	revoked string
}

func (t *runtimePairingTransport) Bootstrap(_ context.Context, _ string, clientPublicKey ed25519.PublicKey) (pairingTarget, pairing.SessionDescriptor, error) {
	serverPublic, serverPrivate, _ := ed25519.GenerateKey(nil)
	t.private = serverPrivate
	descriptor := pairing.SessionDescriptor{
		ID: "session-1", Nonce: []byte("01234567890123456789012345678901"),
		ServerPublicKey: serverPublic, ClientPublicKey: append(ed25519.PublicKey(nil), clientPublicKey...),
		ExpiresAt: time.Now().Add(time.Minute),
	}
	return pairingTarget{Name: "Dev PC", Address: "192.168.1.20", PairingPort: 43119}, descriptor, nil
}

func (t *runtimePairingTransport) Confirm(_ context.Context, _ pairingTarget, descriptor pairing.SessionDescriptor, clientDeviceID, authorizedKey, code string) (pairing.DeviceRecord, error) {
	want, _ := pairing.Code(descriptor)
	if code != want || clientDeviceID == "" || !strings.HasPrefix(authorizedKey, "ssh-ed25519 ") {
		return pairing.DeviceRecord{}, errors.New("invalid confirmation")
	}
	return pairing.DeviceRecord{
		DeviceID: clientDeviceID, AuthorizedKeys: []string{authorizedKey},
		SSHHostPublicKey: t.hostKey, SyncthingDeviceID: "WINDOWS-SYNC",
		SSHPort: 49222, SyncthingPort: 49220,
	}, nil
}

func (t *runtimePairingTransport) Revoke(_ context.Context, _ config.Device, clientDeviceID string) error {
	t.revoked = clientDeviceID
	return nil
}

type runtimeDockerExecutor struct{ calls [][]string }

func (e *runtimeDockerExecutor) Run(_ context.Context, invocation dockercli.Invocation) error {
	e.calls = append(e.calls, append([]string(nil), invocation.Args...))
	if len(invocation.Args) >= 2 && invocation.Args[0] == "context" && invocation.Args[1] == "inspect" {
		return runtimeExitError{code: 1}
	}
	return nil
}

type runtimeExitError struct{ code int }

func (e runtimeExitError) Error() string { return "exit" }
func (e runtimeExitError) ExitCode() int { return e.code }

func testAuthorizedKey(t *testing.T) string {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	key, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatalf("NewPublicKey() error = %v", err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

func TestInfrastructureRestorerReplacesLongRunningEventStreamAndReconcilesRelays(t *testing.T) {
	source := &runtimeRelaySource{eventsStarted: make(chan struct{}, 4)}
	sink := &runtimeRelaySink{}
	restorer := newInfrastructureRestorer(func() (portrelay.Reconciler, error) {
		return portrelay.Reconciler{Source: source, Sink: sink, MinBackoff: time.Hour, MaxBackoff: time.Hour}, nil
	})
	lifecycle, stop := context.WithCancel(context.Background())
	defer stop()
	restorer.Bind(lifecycle)
	agent := NewAgent(ObservationFunc(func(context.Context) AgentObservation {
		return AgentObservation{Paired: true, PinnedSSH: true, DockerPing: true, SyncthingConnected: true}
	}), restorer, nil)

	if err := agent.Reconnect(context.Background()); err != nil {
		t.Fatalf("first Reconnect() error = %v", err)
	}
	select {
	case <-source.eventsStarted:
	case <-time.After(time.Second):
		t.Fatal("first Docker event stream did not start")
	}
	if err := agent.Reconnect(context.Background()); err != nil {
		t.Fatalf("second Reconnect() error = %v", err)
	}
	select {
	case <-source.eventsStarted:
	case <-time.After(time.Second):
		t.Fatal("replacement Docker event stream did not start")
	}
	if sink.Calls() < 2 {
		t.Fatalf("relay reconciles = %d, want at least 2", sink.Calls())
	}
}

type runtimeRelaySource struct{ eventsStarted chan struct{} }

func (*runtimeRelaySource) RunningContainers(context.Context) ([]portrelay.Container, error) {
	return []portrelay.Container{{ID: "running", Running: true}}, nil
}

func (s *runtimeRelaySource) Events(ctx context.Context) (<-chan portrelay.Event, error) {
	stream := make(chan portrelay.Event)
	s.eventsStarted <- struct{}{}
	go func() {
		defer close(stream)
		<-ctx.Done()
	}()
	return stream, nil
}

type runtimeRelaySink struct {
	calls atomic.Int32
}

func (s *runtimeRelaySink) Apply(context.Context, portrelay.Snapshot) error {
	s.calls.Add(1)
	return nil
}

func (s *runtimeRelaySink) Calls() int {
	return int(s.calls.Load())
}
