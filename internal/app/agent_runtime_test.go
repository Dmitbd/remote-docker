package app

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/credentials"
	"github.com/Dmitbd/remote-docker/internal/diagnostics"
	"github.com/Dmitbd/remote-docker/internal/discovery"
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
	controller, ok := runtimeAgent.agent.controller.(*productionAgentController)
	preparer, prepared := controller.dockerPreparer.(*productionDockerPreparer)
	if !ok || !prepared || preparer.sync == nil {
		t.Fatal("production runtime did not wire the Docker preflight preparer")
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
	var recovered localapi.RecoverResult
	err = client.Call(ctx, localapi.MethodRecover, nil, &recovered)
	if err != nil {
		t.Fatalf("Recover error = %v", err)
	}
	if recovered.State != string(AgentUnpaired) || recovered.Message != "pair a device to continue" || len(recovered.Attempts) == 0 {
		t.Fatalf("Recover result = %#v, want safe unpaired state and attempts", recovered)
	}

	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestAgentRuntimeRunInvokesBoundedStartupRecoveryOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	restorer := newInfrastructureRestorer(func() (portrelay.Reconciler, error) {
		return portrelay.Reconciler{}, nil
	})
	agent := NewAgent(nil, restorer, nil)
	localSync := &recordingLocalSyncLifecycle{started: make(chan struct{}), stopped: make(chan struct{})}
	calls := 0
	runtimeAgent := &AgentRuntime{
		agent: agent, restorer: restorer, ssh: &managedSSHRuntime{}, localSync: localSync,
		startupRecover: func(recoveryCtx context.Context) error {
			calls++
			if _, ok := recoveryCtx.Deadline(); !ok {
				t.Fatal("startup recovery context has no deadline")
			}
			cancel()
			return nil
		},
	}
	if err := runtimeAgent.Run(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
	if calls != 1 {
		t.Fatalf("startup recovery calls = %d, want one bounded self-heal wave", calls)
	}
	select {
	case <-localSync.started:
	default:
		t.Fatal("local Syncthing lifecycle was not started")
	}
	select {
	case <-localSync.stopped:
	default:
		t.Fatal("local Syncthing lifecycle was not stopped")
	}
}

type recordingLocalSyncLifecycle struct {
	started chan struct{}
	stopped chan struct{}
	once    sync.Once
}

func (r *recordingLocalSyncLifecycle) Run(ctx context.Context, _ time.Duration) error {
	r.once.Do(func() { close(r.started) })
	<-ctx.Done()
	close(r.stopped)
	return ctx.Err()
}

func TestStartupRecoveryUsesTypedDiagnosticsOnlyOnWindows(t *testing.T) {
	reconnects := 0
	selfHeals := 0
	reconnect := func(context.Context) error { reconnects++; return nil }
	selfHeal := func(context.Context) error { selfHeals++; return nil }

	if err := selectStartupRecovery("windows", reconnect, selfHeal)(context.Background()); err != nil {
		t.Fatalf("Windows startup recovery error = %v", err)
	}
	if reconnects != 0 || selfHeals != 1 {
		t.Fatalf("Windows startup calls reconnect=%d self-heal=%d, want diagnostics self-heal only", reconnects, selfHeals)
	}
	if err := selectStartupRecovery("darwin", reconnect, selfHeal)(context.Background()); err != nil {
		t.Fatalf("Darwin startup recovery error = %v", err)
	}
	if reconnects != 1 || selfHeals != 1 {
		t.Fatalf("Darwin startup calls reconnect=%d self-heal=%d, want reconnect only", reconnects, selfHeals)
	}
}

func TestProductionAgentControllerPreparesDockerWithoutExecutingUserCommand(t *testing.T) {
	preparer := &recordingDockerPreparer{}
	controller := &productionAgentController{dockerPreparer: preparer}
	raw := []byte(`{"bind_sources":["/Users/demo/project"],"working_directory":"/Users/demo/project"}`)

	result, err := controller.Handle(context.Background(), localapi.MethodPrepareDocker, raw)
	if err != nil {
		t.Fatalf("PrepareDocker error = %v", err)
	}
	if result != (localapi.PrepareDockerResult{Ready: true}) {
		t.Fatalf("PrepareDocker result = %#v", result)
	}
	if !reflect.DeepEqual(preparer.params.BindSources, []string{"/Users/demo/project"}) ||
		preparer.params.WorkingDirectory != "/Users/demo/project" {
		t.Fatalf("preparer params = %#v", preparer.params)
	}
}

type recordingDockerPreparer struct {
	params localapi.PrepareDockerParams
}

func (p *recordingDockerPreparer) Prepare(_ context.Context, params localapi.PrepareDockerParams) error {
	p.params = params
	return nil
}

func TestProductionDiagnosticsReturnsOrderedSafeChecks(t *testing.T) {
	checks := newProductionDiagnosticsWithOptions(productionDiagnosticsOptions{
		Observe: func(context.Context) AgentStatus {
			return AgentStatus{State: AgentReady, Message: "Bearer not-a-secret-in-output"}
		},
		Remote: staticRemoteDiagnostics{status: remoteDiagnosticStatus{
			WSLRunning: true, SystemdTarget: true, DiskAvailable: true,
		}},
		PortRelays: diagnostics.CheckFunc(func(context.Context) error { return nil }),
		Platform:   "darwin",
	}).Doctor(context.Background()).Checks
	wantNames := []string{
		"lan_reachability", "ssh_identity", "wsl_running", "systemd_target",
		"docker_socket", "disk", "syncthing", "port_relays",
	}
	if len(checks) != len(wantNames) {
		t.Fatalf("check count = %d, want %d", len(checks), len(wantNames))
	}
	for index, want := range wantNames {
		if checks[index].Name != want {
			t.Fatalf("check[%d].Name = %q, want %q", index, checks[index].Name, want)
		}
		if strings.Contains(checks[index].Message, "not-a-secret-in-output") {
			t.Fatalf("check[%d] leaked observer message: %#v", index, checks[index])
		}
	}
	for index, check := range checks {
		if !check.OK || check.Message != "" {
			t.Fatalf("check[%d] = %#v, want production-ready operation", index, check)
		}
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
		ClientDeviceID:  func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:   filepath.Join(root, "ssh_config"),
		KnownHostsPath:  filepath.Join(root, "known_hosts"),
		AgentSocketPath: filepath.Join(root, "ssh-agent.sock"),
		ControlDir:      filepath.Join(root, "control"),
	})
	candidates, err := coordinator.Candidates(context.Background())
	if err != nil {
		t.Fatalf("Candidates() error = %v", err)
	}
	if want := []localapi.PairingCandidate{{ID: "windows-peer", Name: "Dev PC", Unverified: true}}; !reflect.DeepEqual(candidates.Candidates, want) {
		t.Fatalf("candidates = %#v, want %#v", candidates.Candidates, want)
	}

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
		device.SyncthingDeviceID != "WINDOWS-SYNC" || device.ClientDeviceID != "LOCAL-SYNC" ||
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

func (*runtimePairingTransport) Candidates(context.Context) ([]pairingTarget, error) {
	return []pairingTarget{{Name: "Dev PC", InstanceID: "windows-peer", Address: "192.168.1.20", PairingPort: 43119}}, nil
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

func TestWindowsPairingHostRetriesAndPublishesOnReachableLANInterface(t *testing.T) {
	host, err := newWindowsPairingHost(runtimePairingInstaller{})
	if err != nil {
		t.Fatalf("newWindowsPairingHost() error = %v", err)
	}
	publisher := &retryingPairingPublisher{published: make(chan discovery.Advertisement, 1)}
	host.publisher = publisher
	host.minRetryBackoff = time.Millisecond
	host.maxRetryBackoff = 2 * time.Millisecond
	host.republishInterval = time.Hour
	var listenAddress string
	var listenerPort atomic.Int32
	var listenCalls atomic.Int32
	host.listen = func(_ string, address string) (net.Listener, error) {
		listenAddress = address
		if listenCalls.Add(1) == 1 {
			return nil, errors.New("private LAN is not available yet")
		}
		listenerPort.Store(43119)
		return newBlockingPairingListener(43119), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		host.Run(ctx)
		close(done)
	}()

	var advertisement discovery.Advertisement
	select {
	case advertisement = <-publisher.published:
	case <-time.After(time.Second):
		cancel()
		t.Fatalf(
			"pairing host did not retry the temporary publication failure: listens=%d publishes=%d port=%d",
			listenCalls.Load(), publisher.calls.Load(), listenerPort.Load(),
		)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pairing host did not stop after cancellation")
	}

	if listenAddress != ":0" {
		t.Fatalf("pairing listen address = %q, want wildcard ephemeral port", listenAddress)
	}
	if publisher.calls.Load() != 2 {
		t.Fatalf("publish calls = %d, want one failure and one retry", publisher.calls.Load())
	}
	if listenCalls.Load() != 2 {
		t.Fatalf("listen calls = %d, want no-network failure and retry", listenCalls.Load())
	}
	if advertisement.Port != int(listenerPort.Load()) {
		t.Fatalf("published port = %d, want reachable listener port %d", advertisement.Port, listenerPort.Load())
	}
	if want := []string{"version=1", "instance=" + host.server.InstanceID(), "pairing=1"}; !reflect.DeepEqual(advertisement.TXT, want) {
		t.Fatalf("advertisement TXT = %#v, want opaque TLS-bound data %#v", advertisement.TXT, want)
	}
}

func TestPrivatePeerListenerRejectsPublicPeerAtAcceptBoundary(t *testing.T) {
	public := &addressedConn{remote: &net.TCPAddr{IP: net.ParseIP("203.0.113.10"), Port: 43119}}
	private := &addressedConn{remote: &net.TCPAddr{IP: net.ParseIP("192.168.1.20"), Port: 43119}}
	listener := &queuedListener{connections: []net.Conn{public, private}}

	accepted, err := (privatePeerListener{Listener: listener}).Accept()
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	if accepted != private {
		t.Fatalf("Accept() returned %v, want private peer", accepted.RemoteAddr())
	}
	if public.closes.Load() != 1 {
		t.Fatalf("public peer closes = %d, want 1", public.closes.Load())
	}
}

func TestManagedSSHRuntimeEnsureRestartsDeadAgentForReconnect(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		ActiveDevice:  "pc-1",
		Devices: map[string]config.Device{
			"pc-1": {Address: "192.168.1.20", SSHPort: 49222},
		},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	secrets := credentials.NewMemoryStore()
	if err := secrets.Put("pc-1", sshtransport.SSHPrivateKeyCredential, []byte("private-key")); err != nil {
		t.Fatalf("seed private key: %v", err)
	}

	var agents []*recordingManagedSSHAgent
	sshRuntime := &managedSSHRuntime{
		store: store, secrets: secrets,
		sshConfigPath:   filepath.Join(root, "ssh_config"),
		knownHostsPath:  filepath.Join(root, "known_hosts"),
		agentSocketPath: filepath.Join(root, "agent", "ssh-agent.sock"),
		controlDir:      filepath.Join(root, "control"),
		start: func(context.Context, string, []byte) (managedSSHAgent, error) {
			agent := &recordingManagedSSHAgent{}
			agents = append(agents, agent)
			return agent, nil
		},
		probe: func(context.Context, string) error {
			return errors.New("managed ssh-agent socket is stale")
		},
	}

	if err := sshRuntime.Ensure(context.Background()); err != nil {
		t.Fatalf("first Ensure() error = %v", err)
	}
	if err := sshRuntime.Ensure(context.Background()); err != nil {
		t.Fatalf("reconnect Ensure() error = %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("managed agent starts = %d, want 2", len(agents))
	}
	if agents[0].closes.Load() != 1 {
		t.Fatalf("dead managed agent closes = %d, want 1", agents[0].closes.Load())
	}
}

type runtimePairingInstaller struct{}

func (runtimePairingInstaller) Install(context.Context, string, string) (pairing.DeviceInfo, error) {
	return pairing.DeviceInfo{}, nil
}

func (runtimePairingInstaller) Revoke(context.Context, string) error { return nil }

type retryingPairingPublisher struct {
	calls     atomic.Int32
	published chan discovery.Advertisement
}

func (p *retryingPairingPublisher) Publish(_ context.Context, advertisement discovery.Advertisement) (discovery.Registration, error) {
	if p.calls.Add(1) == 1 {
		return nil, errors.New("temporary mDNS failure")
	}
	p.published <- advertisement
	return &recordingPairingRegistration{}, nil
}

type recordingPairingRegistration struct{ shutdowns atomic.Int32 }

func (r *recordingPairingRegistration) Shutdown() { r.shutdowns.Add(1) }

type queuedListener struct {
	mu          sync.Mutex
	connections []net.Conn
}

func (l *queuedListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.connections) == 0 {
		return nil, net.ErrClosed
	}
	connection := l.connections[0]
	l.connections = l.connections[1:]
	return connection, nil
}

func (*queuedListener) Close() error   { return nil }
func (*queuedListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4zero} }

type blockingPairingListener struct {
	closeOnce sync.Once
	closed    chan struct{}
	address   *net.TCPAddr
}

func newBlockingPairingListener(port int) *blockingPairingListener {
	return &blockingPairingListener{
		closed:  make(chan struct{}),
		address: &net.TCPAddr{IP: net.IPv4zero, Port: port},
	}
}

func (l *blockingPairingListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *blockingPairingListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *blockingPairingListener) Addr() net.Addr { return l.address }

type addressedConn struct {
	net.Conn
	remote net.Addr
	closes atomic.Int32
}

func (c *addressedConn) Close() error {
	c.closes.Add(1)
	return nil
}

func (c *addressedConn) RemoteAddr() net.Addr { return c.remote }

type recordingManagedSSHAgent struct{ closes atomic.Int32 }

func (a *recordingManagedSSHAgent) Close() error {
	a.closes.Add(1)
	return nil
}
