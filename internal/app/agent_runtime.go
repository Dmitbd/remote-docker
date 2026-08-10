package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/credentials"
	"github.com/Dmitbd/remote-docker/internal/diagnostics"
	"github.com/Dmitbd/remote-docker/internal/discovery"
	"github.com/Dmitbd/remote-docker/internal/dockercli"
	"github.com/Dmitbd/remote-docker/internal/lifecycle"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/metrics"
	"github.com/Dmitbd/remote-docker/internal/pairing"
	"github.com/Dmitbd/remote-docker/internal/portrelay"
	"github.com/Dmitbd/remote-docker/internal/provision"
	"github.com/Dmitbd/remote-docker/internal/sshtransport"
	"github.com/Dmitbd/remote-docker/internal/syncer"
	"github.com/Dmitbd/remote-docker/internal/systemtransport"
	"github.com/Dmitbd/remote-docker/internal/tunnel"
	"github.com/Dmitbd/remote-docker/internal/windowsbridge"
	"github.com/Dmitbd/remote-docker/internal/workspace"
	"golang.org/x/crypto/ssh"
	sshagent "golang.org/x/crypto/ssh/agent"
)

const (
	agentProbeTimeout            = 5 * time.Second
	startupRecoveryTimeout       = 30 * time.Second
	managedSSHProbeTimeout       = time.Second
	pairingHostMinRetryBackoff   = 250 * time.Millisecond
	pairingHostMaxRetryBackoff   = 5 * time.Second
	pairingHostRepublishInterval = 30 * time.Second
	windowsPairingListenAddress  = ":49221"
)

// ProductionAgentOptions identifies the installed executable and persisted
// non-secret config. Empty platform paths use per-user defaults.
type ProductionAgentOptions struct {
	ConfigPath          string
	ExecutablePath      string
	SyncthingExecutable string
}

type localSyncLifecycle interface {
	Run(context.Context, time.Duration) error
}

// AgentRuntime owns the concrete controller, health observer, pairing host,
// SSH child, Docker event reconciler, and relay state for one background agent.
type AgentRuntime struct {
	agent          *Agent
	store          config.Store
	sshConfigPath  string
	restorer       *infrastructureRestorer
	pairHost       *windowsPairingHost
	ssh            *managedSSHRuntime
	localSync      localSyncLifecycle
	windowsBridge  localSyncLifecycle
	tunnelClient   localSyncLifecycle
	tunnelReady    *atomic.Bool
	windowsStopper managedWindowsRuntimeStopper
	connection     connectionSessionRuntime
	startupRecover func(context.Context) error

	sessionMu      sync.Mutex
	sessionCancel  context.CancelFunc
	sessionWait    chan struct{}
	sessionDone    chan error
	sessionErr     error
	sessionRunning bool
}

type managedWindowsRuntimeStopper interface {
	StopManagedRuntime(context.Context) (windowsbridge.StopReport, error)
}

// NewProductionAgentRuntime builds the real platform composition.
func NewProductionAgentRuntime(options ProductionAgentOptions) (*AgentRuntime, error) {
	configPath := options.ConfigPath
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("find user home: %w", err)
		}
		configPath = config.DefaultPath(runtime.GOOS, home)
	}
	if !filepath.IsAbs(configPath) {
		return nil, errors.New("background agent config path must be absolute")
	}
	executablePath := options.ExecutablePath
	if executablePath == "" {
		var err error
		executablePath, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("find background agent executable: %w", err)
		}
	}
	managedSSHRoot, err := sshtransport.NewManagedRoot(filepath.Dir(configPath))
	if err != nil {
		return nil, fmt.Errorf("prepare managed SSH root: %w", err)
	}
	_, knownHostsPath, agentSocketPath, controlDir := defaultRuntimePaths(configPath)
	sshConfigPath := managedSSHRoot.SSHConfigPath()
	store := config.Store{Path: configPath}
	configTransactions := &configTransactions{}
	secrets := credentials.NewKeyringStore()
	dockerCLI := realDockerCLIPath(executablePath)
	dockerEnv, err := sshtransport.ManagedDockerEnvironment(os.Environ(), dockerCLI, sshConfigPath)
	if err != nil {
		return nil, fmt.Errorf("prepare managed Docker environment: %w", err)
	}
	httpClient := &http.Client{Timeout: agentProbeTimeout}
	syncHTTPClient := &http.Client{Timeout: defaultPreflightTimeout}
	sshRuntime := &managedSSHRuntime{
		store: store, secrets: secrets,
		sshConfigPath: sshConfigPath, knownHostsPath: knownHostsPath,
		agentSocketPath: agentSocketPath, controlDir: controlDir,
	}

	var pairingCoordinator runtimePairingCoordinator
	var pairHost *windowsPairingHost
	var localSync localSyncLifecycle
	var windowsBridge localSyncLifecycle
	var tunnelClient localSyncLifecycle
	var tunnelReady *atomic.Bool
	if runtime.GOOS == "windows" {
		installer := provision.WSLPairingInstaller{}
		registry := windowsPairingRegistry{store: store, configTransactions: configTransactions}
		identity, identityErr := tunnel.LoadOrCreateIdentity(secrets, tunnel.WindowsIdentityOwner)
		if identityErr != nil {
			return nil, identityErr
		}
		host, err := newWindowsPairingHostWithRegistryAndIdentity(installer, registry, identity)
		if err != nil {
			return nil, err
		}
		pairHost = host
		tunnelTLSConfig, tunnelTLSErr := tunnel.ServerTLSConfig(identity, func(publicKey ed25519.PublicKey) bool {
			cfg, loadErr := loadAgentConfig(store)
			if loadErr != nil || cfg.ActiveDevice == "" {
				return false
			}
			device, ok := cfg.Devices[cfg.ActiveDevice]
			if !ok || device.TransportVersion != tunnel.CurrentTransportVersion {
				return false
			}
			pinned, parseErr := tunnel.ParsePublicKey(device.TunnelPeerPublicKey)
			return parseErr == nil && subtle.ConstantTimeCompare(pinned, publicKey) == 1
		})
		if tunnelTLSErr != nil {
			return nil, tunnelTLSErr
		}
		serviceDialer := windowsbridge.NewProductionServiceDialer()
		pairHost.tunnelTLSConfig = tunnelTLSConfig
		pairHost.serveTunnel = func(ctx context.Context, listener net.Listener) error {
			server := tunnel.Server{
				Accept: func(context.Context) (tunnel.Session, error) {
					connection, acceptErr := listener.Accept()
					if acceptErr != nil {
						return nil, acceptErr
					}
					session, sessionErr := tunnel.NewServerSession(connection)
					if sessionErr != nil {
						_ = connection.Close()
						return nil, sessionErr
					}
					return session, nil
				},
				Dialer: serviceDialer,
			}
			return server.Run(ctx)
		}
		pairingCoordinator = windowsPairingCoordinator{server: host.server, installer: installer, registry: &registry}
		managedRuntime, runtimeErr := provision.NewManagedWSLRuntime(executablePath, secrets)
		if runtimeErr != nil {
			return nil, runtimeErr
		}
		localSync = managedRuntime
	} else {
		syncthingExecutable := options.SyncthingExecutable
		if syncthingExecutable == "" {
			syncthingExecutable = "/usr/local/libexec/remote-docker/syncthing"
		}
		applicationRoot := filepath.Dir(configPath)
		macSync := newLocalSyncthingRuntime(localSyncthingOptions{
			Store: store, Secrets: secrets, Executable: syncthingExecutable,
			ConfigTransactions:  configTransactions,
			PersistentConfigDir: filepath.Join(applicationRoot, "syncthing", "config"),
			DataDir:             filepath.Join(applicationRoot, "syncthing", "data"),
			RuntimeRoot:         filepath.Join(applicationRoot, "run", "syncthing"),
			HTTPClient:          httpClient,
		})
		pairingCoordinator = newMacPairingCoordinator(macPairingOptions{
			Store: store, Secrets: secrets,
			ConfigTransactions: configTransactions,
			Transport: discoveryPairingTransport{
				SSHConfigPath: sshConfigPath,
				DialContext:   systemtransport.PairingDialContext(),
			},
			Docker: dockercli.Runner{}, DockerCLI: dockerCLI, DockerContext: defaultContextName,
			SSHConfigPath: sshConfigPath, KnownHostsPath: knownHostsPath,
			ManagedSSHRoot:  managedSSHRoot,
			AgentSocketPath: agentSocketPath, ControlDir: controlDir,
			ClientDeviceID: macSync.DeviceID,
		})
		localSync = macSync
		tunnelReady = &atomic.Bool{}
		tunnelClient = tunnelClientLifecycle{client: newProductionTunnelClient(store, secrets, func(state tunnel.ClientState, _ error) {
			tunnelReady.Store(state == tunnel.ClientConnected)
		})}
	}

	observer := &productionAgentObserver{
		store: store, knownHostsPath: knownHostsPath,
		dockerCLI: dockerCLI, dockerContext: defaultContextName, dockerEnv: dockerEnv,
		secrets: secrets, httpClient: httpClient,
	}
	restorer := newInfrastructureRestorer(func(ctx context.Context) (portrelay.Reconciler, error) {
		if runtime.GOOS == "windows" {
			return portrelay.Reconciler{}, unavailable("port relay runtime is available on macOS")
		}
		cfg, err := loadAgentConfig(store)
		if err != nil {
			return portrelay.Reconciler{}, needsAction("background agent configuration is unavailable")
		}
		if cfg.ActiveDevice == "" {
			return portrelay.Reconciler{}, needsAction("pair a device before restoring relays")
		}
		device, ok := cfg.Devices[cfg.ActiveDevice]
		if !ok {
			return portrelay.Reconciler{}, needsAction("active paired device is missing")
		}
		if err := sshRuntime.Ensure(ctx); err != nil {
			return portrelay.Reconciler{}, unavailable("managed SSH runtime is unavailable")
		}
		alias := "remote-docker-device-" + cfg.ActiveDevice
		source := portrelay.DockerSource{CLI: dockerCLI, Context: defaultContextName, Env: dockerEnv}
		supervisor := portrelay.NewSupervisor(portrelay.SSHForwardStarter{
			Forwarder:  sshtransport.Forwarder{Binary: "/usr/bin/ssh", Env: os.Environ()},
			ConfigPath: sshConfigPath, ManagedHost: alias,
		}, 250*time.Millisecond, 5*time.Second)
		_ = device
		return portrelay.Reconciler{Source: source, Sink: supervisor}, nil
	})
	syncInspector := productionSyncInspector{secrets: secrets, httpClient: httpClient}
	var syncReadiness SyncReadiness
	if runtime.GOOS != "windows" {
		syncReadiness = productionSyncReadiness{
			store: store, secrets: secrets, httpClient: syncHTTPClient,
			remote: sshRemoteSync{store: store, sshConfigPath: sshConfigPath},
		}
	}
	var remoteMetrics metrics.RemoteSampler
	if runtime.GOOS != "windows" {
		remoteMetrics = sshRemoteMetrics{store: store, sshConfigPath: sshConfigPath}
	}
	controller := &productionAgentController{
		store: store, pairing: pairingCoordinator, configTransactions: configTransactions,
		sync:           syncInspector,
		dockerPreparer: &productionDockerPreparer{store: store, sync: syncReadiness},
		metrics:        metrics.NewCollector(metrics.Options{Remote: remoteMetrics}),
	}
	agent := NewAgent(observer, restorer, controller)
	controller.diagnostics = newProductionDiagnosticsWithOptions(productionDiagnosticsOptions{
		Observe: agent.Refresh, Reconnect: agent.Reconnect, Platform: runtime.GOOS,
		Remote:  sshRemoteDiagnostics{store: store, sshConfigPath: sshConfigPath},
		Windows: windowsbridge.ManagedWSLOperations{},
	})
	if runtime.GOOS == "windows" {
		controller.diagnostics.options.Remote = nil
	} else {
		controller.diagnostics.options.Windows = nil
		controller.diagnostics.options.RestartUserProcess = sshRuntime.Restart
		controller.diagnostics.options.ReconcileAfterRepair = agent.Reconnect
		controller.diagnostics.options.PortRelays = diagnostics.CheckFunc(restorer.CheckPortRelays)
	}
	controller.afterPair = func(ctx context.Context) { _ = agent.Reconnect(ctx) }
	startupSelfHeal := func(ctx context.Context) error {
		_, _, err := controller.diagnostics.Recover(ctx)
		return err
	}
	return &AgentRuntime{
		agent: agent, store: store, sshConfigPath: sshConfigPath,
		restorer: restorer, pairHost: pairHost, ssh: sshRuntime, localSync: localSync,
		windowsBridge: windowsBridge,
		tunnelClient: tunnelClient,
		tunnelReady: tunnelReady,
		windowsStopper: func() managedWindowsRuntimeStopper {
			if runtime.GOOS == "windows" {
				return windowsbridge.ManagedWSLOperations{}
			}
			return nil
		}(),
		startupRecover: selectStartupRecovery(runtime.GOOS, agent.Reconnect, startupSelfHeal),
	}, nil
}

// Agent returns the concrete local API handler.
func (r *AgentRuntime) Agent() *Agent { return r.agent }

// BindLifecycle connects the production transport state to the product-facing
// desktop machine. It must be called once before the session is started.
func (r *AgentRuntime) BindLifecycle(machine *lifecycle.Machine, appVersion string) error {
	if r == nil || machine == nil {
		return errors.New("production lifecycle binding is incomplete")
	}
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	if r.sessionRunning {
		return errors.New("production lifecycle cannot be rebound while running")
	}
	snapshot := machine.Snapshot()
	if strings.TrimSpace(appVersion) == "" {
		appVersion = "dev"
	}
	if snapshot.Role == lifecycle.RoleWindowsHost {
		host, err := newHostConnectionRuntime(machine, windowsbridge.ManagedWSLOperations{}, time.Now)
		if err != nil {
			return err
		}
		r.connection = host
		return nil
	}
	if snapshot.Role != lifecycle.RoleMacClient {
		return errors.New("production lifecycle role is invalid")
	}
	r.connection = &clientConnectionRuntime{
		machine: machine,
		ready: func() bool {
			return r.agent != nil && r.agent.Status().State == AgentReady && r.tunnelReady != nil && r.tunnelReady.Load()
		},
		clientDeviceID: func() string {
			cfg, err := loadAgentConfig(r.store)
			if err != nil {
				return ""
			}
			return cfg.LocalSyncthingDeviceID
		},
		localName: snapshot.LocalName, appVersion: appVersion,
		transport: func(ctx context.Context) (PresenceTransport, error) {
			return newProductionSSHPresenceTransport(r.store, r.sshConfigPath), nil
		},
	}
	if tunnelRuntime, ok := r.tunnelClient.(tunnelClientLifecycle); ok && tunnelRuntime.client != nil {
		previous := tunnelRuntime.client.OnState
		tunnelRuntime.client.OnState = func(state tunnel.ClientState, err error) {
			if previous != nil {
				previous(state, err)
			}
			snapshot := machine.Snapshot()
			switch state {
			case tunnel.ClientDisconnected, tunnel.ClientReconnecting:
				if snapshot.State == lifecycle.StateConnected {
					_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventNetworkLost})
				}
			}
		}
	}
	return nil
}

// Start activates one owned runtime session. Construction alone never starts
// infrastructure, which keeps a manually launched desktop application paused.
func (r *AgentRuntime) Start(parent context.Context, role lifecycle.Role) error {
	if r == nil {
		return errors.New("production background agent runtime is nil")
	}
	if role != lifecycle.RoleMacClient && role != lifecycle.RoleWindowsHost {
		return errors.New("production background agent role is invalid")
	}
	if parent == nil {
		parent = context.Background()
	}
	r.sessionMu.Lock()
	if r.sessionRunning {
		r.sessionMu.Unlock()
		return nil
	}
	sessionCtx, cancel := context.WithCancel(parent)
	wait := make(chan struct{})
	done := make(chan error, 1)
	r.sessionCancel = cancel
	r.sessionWait = wait
	r.sessionDone = done
	r.sessionErr = nil
	r.sessionRunning = true
	r.sessionMu.Unlock()

	go func() {
		err := r.Run(sessionCtx, time.Second)
		r.sessionMu.Lock()
		r.sessionErr = err
		r.sessionRunning = false
		close(wait)
		r.sessionMu.Unlock()
		done <- err
		close(done)
	}()
	return nil
}

// Stop cancels the active session and waits for all Run-owned children. The
// reason is intentionally typed even though current cleanup is identical;
// Windows-specific graceful shutdown is added through the same boundary.
func (r *AgentRuntime) Stop(ctx context.Context, reason lifecycle.StopReason) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.sessionMu.Lock()
	cancel := r.sessionCancel
	wait := r.sessionWait
	connection := r.connection
	if cancel == nil || wait == nil {
		r.sessionMu.Unlock()
		return nil
	}
	r.sessionMu.Unlock()
	var presenceErr error
	if connection != nil {
		presenceErr = connection.Stop(ctx, reason)
	}
	cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wait:
		r.sessionMu.Lock()
		runtimeErr := r.sessionErr
		r.sessionCancel = nil
		r.sessionWait = nil
		r.sessionMu.Unlock()
		if errors.Is(runtimeErr, context.Canceled) {
			runtimeErr = nil
		}
		var managedErr error
		if r.windowsStopper != nil {
			_, managedErr = r.windowsStopper.StopManagedRuntime(ctx)
		}
		return errors.Join(presenceErr, runtimeErr, managedErr)
	}
}

func (r *AgentRuntime) Done() <-chan error {
	if r == nil {
		done := make(chan error)
		close(done)
		return done
	}
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	if r.sessionDone == nil {
		done := make(chan error)
		close(done)
		return done
	}
	return r.sessionDone
}

// Run owns all long-running infrastructure until ctx is cancelled.
func (r *AgentRuntime) Run(ctx context.Context, interval time.Duration) error {
	if r == nil || r.agent == nil || r.restorer == nil {
		return errors.New("production background agent runtime is incomplete")
	}
	lifecycleCtx, cancelLifecycle := context.WithCancel(ctx)
	defer cancelLifecycle()
	r.restorer.Bind(lifecycleCtx)
	var pairHostDone chan error
	if r.pairHost != nil {
		pairHostDone = make(chan error, 1)
		go func() {
			r.pairHost.Run(lifecycleCtx)
			pairHostDone <- nil
		}()
	}
	if r.ssh != nil {
		defer r.ssh.Close()
	}
	defer r.restorer.Stop()
	var localSyncDone chan error
	if r.localSync != nil {
		localSyncDone = make(chan error, 1)
		go func() { localSyncDone <- r.localSync.Run(lifecycleCtx, interval) }()
	}
	var connectionDone chan error
	if r.connection != nil {
		connectionDone = make(chan error, 1)
		go func() { connectionDone <- r.connection.Run(lifecycleCtx, interval) }()
	}
	var windowsBridgeDone chan error
	if r.windowsBridge != nil {
		windowsBridgeDone = make(chan error, 1)
		go func() { windowsBridgeDone <- r.windowsBridge.Run(lifecycleCtx, interval) }()
	}
	var tunnelDone chan error
	if r.tunnelClient != nil {
		tunnelDone = make(chan error, 1)
		go func() { tunnelDone <- r.tunnelClient.Run(lifecycleCtx, interval) }()
	}
	recoveryCtx, cancel := context.WithTimeout(lifecycleCtx, startupRecoveryTimeout)
	startupRecover := r.startupRecover
	if startupRecover == nil {
		startupRecover = r.agent.Reconnect
	}
	_ = startupRecover(recoveryCtx)
	cancel()
	if localSyncDone == nil && windowsBridgeDone == nil && connectionDone == nil && tunnelDone == nil {
		err := r.agent.Run(lifecycleCtx, interval)
		cancelLifecycle()
		waitLifecycle(pairHostDone)
		waitLifecycle(tunnelDone)
		return err
	}
	agentDone := make(chan error, 1)
	go func() { agentDone <- r.agent.Run(lifecycleCtx, interval) }()
	select {
	case err := <-agentDone:
		cancelLifecycle()
		waitLifecycle(localSyncDone)
		waitLifecycle(windowsBridgeDone)
		waitLifecycle(connectionDone)
		waitLifecycle(pairHostDone)
		waitLifecycle(tunnelDone)
		return err
	case err := <-localSyncDone:
		cancelLifecycle()
		<-agentDone
		waitLifecycle(windowsBridgeDone)
		waitLifecycle(connectionDone)
		waitLifecycle(pairHostDone)
		waitLifecycle(tunnelDone)
		return err
	case err := <-windowsBridgeDone:
		cancelLifecycle()
		<-agentDone
		waitLifecycle(localSyncDone)
		waitLifecycle(connectionDone)
		waitLifecycle(pairHostDone)
		waitLifecycle(tunnelDone)
		return err
	case err := <-connectionDone:
		cancelLifecycle()
		<-agentDone
		waitLifecycle(localSyncDone)
		waitLifecycle(windowsBridgeDone)
		waitLifecycle(pairHostDone)
		waitLifecycle(tunnelDone)
		return err
	case err := <-tunnelDone:
		cancelLifecycle()
		<-agentDone
		waitLifecycle(localSyncDone)
		waitLifecycle(windowsBridgeDone)
		waitLifecycle(connectionDone)
		waitLifecycle(pairHostDone)
		return err
	}
}

type tunnelClientLifecycle struct {
	client *tunnel.Client
}

func (r tunnelClientLifecycle) Run(ctx context.Context, _ time.Duration) error {
	if r.client == nil {
		return errors.New("Mac tunnel client is unavailable")
	}
	return r.client.Run(ctx)
}

func newProductionTunnelClient(store config.Store, secrets credentials.Store, onState func(tunnel.ClientState, error)) *tunnel.Client {
	return &tunnel.Client{
		OnState: onState,
		Dial: func(ctx context.Context) (net.Conn, error) {
			cfg, err := loadAgentConfig(store)
			if err != nil || cfg.ActiveDevice == "" {
				return nil, errors.New("paired tunnel device is unavailable")
			}
			device, ok := cfg.Devices[cfg.ActiveDevice]
			if !ok || device.TunnelPort != tunnel.TunnelPort || device.TransportVersion != tunnel.CurrentTransportVersion {
				return nil, errors.New("paired tunnel metadata is unavailable")
			}
			encodedIdentity, err := secrets.Get(cfg.ActiveDevice, tunnel.IdentityCredential)
			if err != nil {
				return nil, err
			}
			defer clearSecret(encodedIdentity)
			identity, err := tunnel.IdentityFromPKCS8(encodedIdentity)
			if err != nil {
				return nil, err
			}
			peer, err := tunnel.ParsePublicKey(device.TunnelPeerPublicKey)
			if err != nil {
				return nil, err
			}
			tlsConfig, err := tunnel.ClientTLSConfig(identity, peer)
			if err != nil {
				return nil, err
			}
			raw, err := systemtransport.TunnelDialContext()(ctx, "tcp", net.JoinHostPort(device.Address, "49221"))
			if err != nil {
				return nil, err
			}
			secured := tls.Client(raw, tlsConfig)
			if err := secured.HandshakeContext(ctx); err != nil {
				_ = raw.Close()
				return nil, err
			}
			return secured, nil
		},
		OpenRelays: func(session tunnel.Session) ([]io.Closer, error) {
			listeners, err := tunnel.StartLoopbackRelays(context.Background(), session)
			if err != nil {
				return nil, err
			}
			closers := make([]io.Closer, 0, len(listeners))
			for _, listener := range listeners {
				closers = append(closers, listener)
			}
			return closers, nil
		},
	}
}

func waitLifecycle(done <-chan error) {
	if done != nil {
		<-done
	}
}

func selectStartupRecovery(
	platform string,
	reconnect, windowsSelfHeal func(context.Context) error,
) func(context.Context) error {
	if platform == "windows" && windowsSelfHeal != nil {
		return windowsSelfHeal
	}
	if reconnect != nil {
		return reconnect
	}
	return func(context.Context) error {
		return errors.New("startup recovery is unavailable")
	}
}

type runtimePairingCoordinator interface {
	Candidates(context.Context) (localapi.PairCandidatesResult, error)
	Start(context.Context, string) (localapi.PairStartResult, error)
	Status(context.Context, string) (localapi.PairingStatusResult, error)
	Observe(context.Context, string) (localapi.PairingStatusResult, error)
	Approve(context.Context, string) (localapi.PairingStatusResult, error)
	Reject(context.Context, string) (localapi.PairingStatusResult, error)
	Cancel(context.Context, string) (localapi.PairingStatusResult, error)
	Unpair(context.Context, string, bool) error
}

type configTransactions struct {
	mu sync.Mutex
}

func (t *configTransactions) Run(operation func() error) error {
	if operation == nil {
		return nil
	}
	if t == nil {
		return operation()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return operation()
}

type productionAgentController struct {
	store                   config.Store
	configTransactions      *configTransactions
	pairing                 runtimePairingCoordinator
	sync                    productionSyncInspector
	dockerPreparer          dockerPreparer
	diagnostics             productionDiagnostics
	metrics                 *metrics.Collector
	afterPair               func(context.Context)
	mu                      sync.Mutex
	beforeConfigTransaction func()
	beforeConfigSave        func()
}

func (c *productionAgentController) abandonPairing(sessionID string) {
	if c == nil || c.pairing == nil {
		return
	}
	if cleaner, ok := c.pairing.(interface{ Abandon(string) }); ok {
		cleaner.Abandon(sessionID)
	}
}

type dockerPreparer interface {
	Prepare(context.Context, localapi.PrepareDockerParams) error
}

type productionDockerPreparer struct {
	store config.Store
	sync  SyncReadiness
	ports PortProbe
}

func (p *productionDockerPreparer) Prepare(ctx context.Context, params localapi.PrepareDockerParams) error {
	analysis := dockercli.Analysis{BindSources: append([]string(nil), params.BindSources...)}
	for _, port := range params.StaticTCPPorts {
		analysis.StaticTCPPorts = append(analysis.StaticTCPPorts, dockercli.Port{
			HostIP: port.HostIP, HostPort: port.HostPort, ContainerPort: port.ContainerPort,
		})
	}
	for _, reason := range params.Unsupported {
		analysis.Unsupported = append(analysis.Unsupported, dockercli.Reason{
			Code: dockercli.ReasonCode(reason.Code), Detail: reason.Detail,
		})
	}
	preflight := Preflight{
		Analyzer: preparedDockerAnalysis{analysis: analysis},
		Resolver: storedWorkspaceResolver{store: p.store},
		Sync:     p.sync, Ports: p.ports,
	}
	invocation := dockercli.Invocation{Dir: params.WorkingDirectory}
	if err := preflight.Check(ctx, invocation, nil, io.Discard); err != nil {
		return needsAction(err.Error())
	}
	return nil
}

type preparedDockerAnalysis struct {
	analysis dockercli.Analysis
}

func (a preparedDockerAnalysis) Analyze(context.Context, dockercli.Invocation, dockercli.Executor) (dockercli.Analysis, error) {
	return a.analysis, nil
}

type storedWorkspaceResolver struct {
	store config.Store
}

func (r storedWorkspaceResolver) Resolve(source, cwd string) (workspace.ResolvedPath, error) {
	cfg, err := loadAgentConfig(r.store)
	if err != nil {
		return workspace.ResolvedPath{}, err
	}
	registered := make([]workspace.Workspace, 0, len(cfg.Workspaces))
	for id, item := range cfg.Workspaces {
		registered = append(registered, workspace.Workspace{ID: id, LocalRoot: item.Path})
	}
	return workspace.ResolveBind(source, cwd, registered)
}

func (c *productionAgentController) Handle(ctx context.Context, method localapi.Method, raw json.RawMessage) (any, error) {
	switch method {
	case localapi.MethodListDevices:
		return c.listDevices()
	case localapi.MethodPairCandidates:
		return c.pairing.Candidates(ctx)
	case localapi.MethodPairStart:
		var params localapi.PairStartParams
		if err := decodeControlParams(raw, &params); err != nil {
			return nil, err
		}
		return c.pairing.Start(ctx, params.Device)
	case localapi.MethodPairStatus:
		var params localapi.PairSessionParams
		if err := decodeControlParams(raw, &params); err != nil {
			return nil, err
		}
		var result localapi.PairingStatusResult
		var err error
		if params.ObserveOnly {
			result, err = c.pairing.Observe(ctx, params.SessionID)
		} else {
			result, err = c.pairing.Status(ctx, params.SessionID)
		}
		if err == nil && !params.ObserveOnly && result.Status == string(pairing.SessionCompleted) && c.afterPair != nil {
			go c.afterPair(context.Background())
		}
		return result, err
	case localapi.MethodPairApprove, localapi.MethodPairReject, localapi.MethodPairCancel:
		var params localapi.PairSessionParams
		if err := decodeControlParams(raw, &params); err != nil {
			return nil, err
		}
		switch method {
		case localapi.MethodPairApprove:
			return c.pairing.Approve(ctx, params.SessionID)
		case localapi.MethodPairReject:
			return c.pairing.Reject(ctx, params.SessionID)
		default:
			return c.pairing.Cancel(ctx, params.SessionID)
		}
	case localapi.MethodUnpair:
		var params localapi.UnpairParams
		if err := decodeControlParams(raw, &params); err != nil {
			return nil, err
		}
		if err := c.pairing.Unpair(ctx, params.DeviceID, params.LocalOnly); err != nil {
			return nil, err
		}
		return map[string]bool{"unpaired": true}, nil
	case localapi.MethodWorkspaceAdd:
		var params localapi.WorkspaceAddParams
		if err := decodeControlParams(raw, &params); err != nil {
			return nil, err
		}
		return c.addWorkspace(params.Path)
	case localapi.MethodWorkspaceList:
		return c.listWorkspaces()
	case localapi.MethodWorkspaceRemove:
		var params localapi.WorkspaceRemoveParams
		if err := decodeControlParams(raw, &params); err != nil {
			return nil, err
		}
		return c.removeWorkspace(params.ID)
	case localapi.MethodSyncStatus:
		cfg, err := loadAgentConfig(c.store)
		if err != nil {
			return nil, unavailable("cannot read sync configuration")
		}
		return c.sync.Status(ctx, cfg)
	case localapi.MethodPrepareDocker:
		var params localapi.PrepareDockerParams
		if err := decodeControlParams(raw, &params); err != nil {
			return nil, err
		}
		if c.dockerPreparer == nil {
			return nil, needsAction("workspace synchronization is not ready")
		}
		if err := c.dockerPreparer.Prepare(ctx, params); err != nil {
			return nil, err
		}
		return localapi.PrepareDockerResult{Ready: true}, nil
	case localapi.MethodDoctor:
		return c.diagnostics.Doctor(ctx), nil
	case localapi.MethodRecover:
		recovery, status, _ := c.diagnostics.Recover(ctx)
		attempts := make([]localapi.RecoverAttempt, 0, len(recovery.Attempts))
		for _, attempt := range recovery.Attempts {
			attempts = append(attempts, localapi.RecoverAttempt{
				Step: string(attempt.Step), OK: attempt.OK, Reason: attempt.Reason,
			})
		}
		return localapi.RecoverResult{
			State: string(status.State), Message: status.Message, Attempts: attempts,
		}, nil
	case localapi.MethodResourceStatus:
		if c.metrics == nil {
			return nil, unavailable("resource monitoring is unavailable")
		}
		params := localapi.ResourceStatusParams{}
		if err := decodeOptionalControlParams(raw, &params); err != nil {
			return nil, err
		}
		return c.metrics.Sample(ctx, params.Active), nil
	default:
		return nil, &localapi.PublicError{Code: localapi.ErrorInvalidRequest, Message: "unsupported agent operation"}
	}
}

func (c *productionAgentController) listDevices() (localapi.ListDevicesResult, error) {
	cfg, err := loadAgentConfig(c.store)
	if err != nil {
		return localapi.ListDevicesResult{}, unavailable("cannot read paired devices")
	}
	result := localapi.ListDevicesResult{Devices: []localapi.Device{}}
	for id, device := range cfg.Devices {
		result.Devices = append(result.Devices, localapi.Device{ID: id, Name: device.Name, Address: device.Address})
	}
	sort.Slice(result.Devices, func(i, j int) bool { return result.Devices[i].ID < result.Devices[j].ID })
	return result, nil
}

func (c *productionAgentController) addWorkspace(path string) (localapi.Workspace, error) {
	canonical, err := canonicalWorkspacePath(path)
	if err != nil {
		return localapi.Workspace{}, &localapi.PublicError{Code: localapi.ErrorInvalidRequest, Message: "workspace must be an existing directory"}
	}
	digest := sha256.Sum256([]byte(canonical))
	id := hex.EncodeToString(digest[:8])
	var result localapi.Workspace
	err = c.runConfigTransaction(func() error {
		cfg, err := loadAgentConfig(c.store)
		if err != nil {
			return unavailable("cannot read workspace configuration")
		}
		if cfg.Workspaces == nil {
			cfg.Workspaces = make(map[string]config.Workspace)
		}
		for existingID, existing := range cfg.Workspaces {
			if existing.Path == canonical {
				result = localapi.Workspace{ID: existingID, Path: canonical}
				return nil
			}
		}
		cfg.Workspaces[id] = config.Workspace{Path: canonical}
		if c.beforeConfigSave != nil {
			c.beforeConfigSave()
		}
		if err := c.store.Save(cfg); err != nil {
			return unavailable("cannot save workspace configuration")
		}
		result = localapi.Workspace{ID: id, Path: canonical}
		return nil
	})
	return result, err
}

func (c *productionAgentController) listWorkspaces() (localapi.WorkspaceListResult, error) {
	cfg, err := loadAgentConfig(c.store)
	if err != nil {
		return localapi.WorkspaceListResult{}, unavailable("cannot read workspace configuration")
	}
	result := localapi.WorkspaceListResult{Workspaces: []localapi.Workspace{}}
	for id, workspace := range cfg.Workspaces {
		result.Workspaces = append(result.Workspaces, localapi.Workspace{ID: id, Path: workspace.Path})
	}
	sort.Slice(result.Workspaces, func(i, j int) bool { return result.Workspaces[i].ID < result.Workspaces[j].ID })
	return result, nil
}

func (c *productionAgentController) removeWorkspace(id string) (map[string]bool, error) {
	if strings.TrimSpace(id) == "" {
		return nil, &localapi.PublicError{Code: localapi.ErrorInvalidRequest, Message: "workspace ID is required"}
	}
	err := c.runConfigTransaction(func() error {
		cfg, err := loadAgentConfig(c.store)
		if err != nil {
			return unavailable("cannot read workspace configuration")
		}
		if _, ok := cfg.Workspaces[id]; !ok {
			return needsAction("workspace was not found")
		}
		delete(cfg.Workspaces, id)
		if c.beforeConfigSave != nil {
			c.beforeConfigSave()
		}
		if err := c.store.Save(cfg); err != nil {
			return unavailable("cannot save workspace configuration")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]bool{"removed": true}, nil
}

func (c *productionAgentController) runConfigTransaction(operation func() error) error {
	if c.beforeConfigTransaction != nil {
		c.beforeConfigTransaction()
	}
	if c.configTransactions != nil {
		return c.configTransactions.Run(operation)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return operation()
}

func decodeControlParams(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return &localapi.PublicError{Code: localapi.ErrorInvalidRequest, Message: "invalid agent operation parameters"}
	}
	return nil
}

func canonicalWorkspacePath(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return "", errors.New("workspace is not a directory")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Abs(canonical)
}

type productionAgentObserver struct {
	store          config.Store
	knownHostsPath string
	dockerCLI      string
	dockerContext  string
	dockerEnv      []string
	secrets        credentials.Store
	httpClient     *http.Client
}

func (o *productionAgentObserver) Observe(ctx context.Context) AgentObservation {
	cfg, err := loadAgentConfig(o.store)
	if err != nil {
		return AgentObservation{Err: err}
	}
	if cfg.ActiveDevice == "" {
		return AgentObservation{}
	}
	device, ok := cfg.Devices[cfg.ActiveDevice]
	if !ok {
		return AgentObservation{Paired: true, NeedsAction: "active paired device is missing"}
	}
	observation := AgentObservation{Paired: true}
	if strings.TrimSpace(device.SSHHostPublicKey) == "" || strings.TrimSpace(device.SyncthingDeviceID) == "" {
		observation.NeedsAction = "paired device metadata is incomplete; pair the device again"
		return observation
	}
	alias := "remote-docker-device-" + cfg.ActiveDevice
	pinned, err := knownHostPinned(o.knownHostsPath, alias, device.SSHHostPublicKey)
	if err != nil || !pinned {
		return observation
	}
	observation.PinnedSSH = true
	probeCtx, cancel := context.WithTimeout(ctx, agentProbeTimeout)
	defer cancel()
	if err := (dockercli.Runner{}).Run(probeCtx, dockercli.Invocation{
		Binary: o.dockerCLI,
		Args:   []string{"--context", o.dockerContext, "version", "--format", "{{.Server.Version}}"},
		Env:    o.dockerEnv,
		Stdout: io.Discard, Stderr: io.Discard,
	}); err != nil {
		return observation
	}
	observation.DockerPing = true
	inspector := productionSyncInspector{secrets: o.secrets, httpClient: o.httpClient}
	connected, err := inspector.Connected(probeCtx, cfg)
	if err == nil {
		observation.SyncthingConnected = connected
	}
	return observation
}

func knownHostPinned(path, alias, expected string) (bool, error) {
	expectedKey, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(expected))
	if err != nil || len(bytes.TrimSpace(rest)) != 0 {
		return false, errors.New("expected SSH host key is invalid")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	for len(contents) > 0 {
		_, hosts, key, _, remaining, parseErr := ssh.ParseKnownHosts(contents)
		if parseErr != nil {
			return false, parseErr
		}
		contents = remaining
		for _, host := range hosts {
			if host == alias && bytes.Equal(key.Marshal(), expectedKey.Marshal()) {
				return true, nil
			}
		}
	}
	return false, nil
}

type productionSyncInspector struct {
	secrets    credentials.Store
	httpClient *http.Client
	endpoint   string
}

func (i productionSyncInspector) client(cfg config.Config) (*syncer.Client, config.Device, error) {
	_, ok := cfg.Devices[cfg.ActiveDevice]
	if cfg.ActiveDevice == "" || !ok || strings.TrimSpace(device.SyncthingDeviceID) == "" {
		return nil, config.Device{}, errors.New("paired Syncthing device is unavailable")
	}
	endpoint := i.endpoint
	if endpoint == "" {
		endpoint = localSyncthingEndpoint
	}
	client, err := syncer.NewClient(endpoint, localSyncthingCredentialOwner, i.secrets, i.httpClient)
	return client, device, err
}

func (i productionSyncInspector) Connected(ctx context.Context, cfg config.Config) (bool, error) {
	client, device, err := i.client(cfg)
	if err != nil {
		return false, err
	}
	connections, err := client.Connections(ctx)
	if err != nil {
		return false, err
	}
	return connections[device.SyncthingDeviceID].Connected, nil
}

func (i productionSyncInspector) Status(ctx context.Context, cfg config.Config) (localapi.SyncStatusResult, error) {
	client, device, err := i.client(cfg)
	if err != nil {
		return localapi.SyncStatusResult{}, unavailable("Syncthing status is unavailable")
	}
	connections, err := client.Connections(ctx)
	if err != nil {
		return localapi.SyncStatusResult{}, unavailable("Syncthing status is unavailable")
	}
	connected := connections[device.SyncthingDeviceID].Connected
	result := localapi.SyncStatusResult{Folders: []localapi.SyncFolderStatus{}}
	ids := make([]string, 0, len(cfg.Workspaces))
	for id := range cfg.Workspaces {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		status, statusErr := client.FolderStatus(ctx, id)
		if statusErr != nil {
			return localapi.SyncStatusResult{}, unavailable("Syncthing folder status is unavailable")
		}
		result.Folders = append(result.Folders, localapi.SyncFolderStatus{
			WorkspaceID: id, State: status.State, Connected: connected,
		})
	}
	return result, nil
}

type infrastructureFactory func(context.Context) (portrelay.Reconciler, error)

type infrastructureRestorer struct {
	factory infrastructureFactory
	mu      sync.Mutex
	life    context.Context
	cancel  context.CancelFunc
	current *portrelay.Reconciler
	runs    sync.WaitGroup
}

func newInfrastructureRestorer(factory infrastructureFactory) *infrastructureRestorer {
	return &infrastructureRestorer{factory: factory}
}

func (r *infrastructureRestorer) Bind(ctx context.Context) {
	r.mu.Lock()
	r.life = ctx
	r.mu.Unlock()
}

func (r *infrastructureRestorer) RestoreEventStream(ctx context.Context) error {
	if r == nil || r.factory == nil {
		return unavailable("Docker event reconciliation is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	reconciler, err := r.factory(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return err
	}
	if r.life == nil || r.life.Err() != nil {
		r.mu.Unlock()
		return unavailable("background agent lifecycle is unavailable")
	}
	if r.cancel != nil {
		r.cancel()
	}
	runCtx, cancel := context.WithCancel(r.life)
	r.cancel = cancel
	r.current = &reconciler
	r.runs.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.runs.Done()
		_ = reconciler.Run(runCtx)
	}()
	return nil
}

func (r *infrastructureRestorer) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel := r.cancel
	r.life = nil
	r.cancel = nil
	r.current = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	r.runs.Wait()
}

func (r *infrastructureRestorer) RestoreRelays(ctx context.Context) error {
	r.mu.Lock()
	current := r.current
	r.mu.Unlock()
	if current == nil {
		return unavailable("port relay reconciliation is unavailable")
	}
	if err := current.Reconcile(ctx); err != nil {
		return unavailable("port relay reconciliation failed")
	}
	return nil
}

func (r *infrastructureRestorer) CheckPortRelays(context.Context) error {
	if r == nil {
		return diagnostics.NewPublicError(diagnostics.ReasonPortRelaysNotReady)
	}
	r.mu.Lock()
	current := r.current
	r.mu.Unlock()
	if current == nil {
		return diagnostics.NewPublicError(diagnostics.ReasonPortRelaysNotReady)
	}
	health, ok := current.Sink.(interface{ Healthy() bool })
	if !ok || !health.Healthy() {
		return diagnostics.NewPublicError(diagnostics.ReasonPortRelaysNotReady)
	}
	return nil
}

type managedSSHRuntime struct {
	store           config.Store
	secrets         credentials.Store
	sshConfigPath   string
	knownHostsPath  string
	agentSocketPath string
	controlDir      string
	start           func(context.Context, string, []byte) (managedSSHAgent, error)
	probe           func(context.Context, string) error
	mu              sync.Mutex
	deviceID        string
	managed         managedSSHAgent
}

type managedSSHAgent interface {
	Close() error
}

func (r *managedSSHRuntime) Ensure(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg, err := loadAgentConfig(r.store)
	if err != nil || cfg.ActiveDevice == "" {
		return errors.New("paired SSH device is unavailable")
	}
	_, ok := cfg.Devices[cfg.ActiveDevice]
	if !ok {
		return errors.New("paired SSH device is unavailable")
	}
	if r.managed != nil && r.deviceID == cfg.ActiveDevice {
		probe := r.probe
		if probe == nil {
			probe = probeManagedSSHAgent
		}
		probeCtx, cancel := context.WithTimeout(ctx, managedSSHProbeTimeout)
		probeErr := probe(probeCtx, r.agentSocketPath)
		cancel()
		if probeErr == nil {
			return nil
		}
	}
	if r.managed != nil {
		_ = r.managed.Close()
		r.managed = nil
		r.deviceID = ""
	}
	if err := os.MkdirAll(filepath.Dir(r.agentSocketPath), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(r.agentSocketPath), 0o700); err != nil {
		return err
	}
	if privateRoot := defaultPrivateRuntimeRoot(); privateRoot != "" && filepath.Clean(filepath.Dir(r.controlDir)) == filepath.Clean(privateRoot) {
		if err := sshtransport.EnsurePrivateDirectory(privateRoot); err != nil {
			return err
		}
	}
	if err := sshtransport.EnsurePrivateDirectory(r.controlDir); err != nil {
		return err
	}
	if err := os.Remove(r.agentSocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	privateKey, err := r.secrets.Get(cfg.ActiveDevice, sshtransport.SSHPrivateKeyCredential)
	if err != nil {
		return err
	}
	start := r.start
	if start == nil {
		start = func(ctx context.Context, socketPath string, privateKey []byte) (managedSSHAgent, error) {
			return (sshtransport.Agent{}).Start(ctx, socketPath, privateKey)
		}
	}
	managed, err := start(ctx, r.agentSocketPath, privateKey)
	if err != nil {
		return err
	}
	if err := sshtransport.WriteConfig(r.sshConfigPath, sshtransport.Config{
		DeviceID: cfg.ActiveDevice, HostName: "127.0.0.1", Port: tunnel.DockerRelayPort,
		AgentSocket: r.agentSocketPath, KnownHostsFile: r.knownHostsPath, ControlDir: r.controlDir,
	}); err != nil {
		_ = managed.Close()
		return err
	}
	r.managed = managed
	r.deviceID = cfg.ActiveDevice
	return nil
}

func (r *managedSSHRuntime) Restart(ctx context.Context) error {
	if r == nil {
		return errors.New("managed SSH runtime is unavailable")
	}
	r.mu.Lock()
	managed := r.managed
	r.managed = nil
	r.deviceID = ""
	r.mu.Unlock()
	if managed != nil {
		if err := managed.Close(); err != nil {
			return errors.New("stop managed SSH user process")
		}
	}
	return r.Ensure(ctx)
}

func probeManagedSSHAgent(ctx context.Context, socketPath string) error {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return err
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return err
		}
	}
	_, err = sshagent.NewClient(connection).List()
	return err
}

func (r *managedSSHRuntime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.managed == nil {
		return nil
	}
	err := r.managed.Close()
	r.managed = nil
	r.deviceID = ""
	return err
}

type windowsPairingHost struct {
	server            *pairing.Server
	publisher         discovery.Publisher
	listen            func(string, string) (net.Listener, error)
	minRetryBackoff   time.Duration
	maxRetryBackoff   time.Duration
	republishInterval time.Duration
	tunnelTLSConfig   *tls.Config
	serveTunnel       func(context.Context, net.Listener) error
}

func newWindowsPairingHost(installer pairing.Installer) (*windowsPairingHost, error) {
	return newWindowsPairingHostWithRegistry(installer, windowsPairingRegistry{})
}

func newWindowsPairingHostWithRegistry(installer pairing.Installer, registry windowsPairingRegistry) (*windowsPairingHost, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return newWindowsPairingHostWithRegistryAndIdentity(installer, registry, tunnel.Identity{
		PrivateKey: privateKey, PublicKey: publicKey,
	})
}

func newWindowsPairingHostWithRegistryAndIdentity(installer pairing.Installer, registry windowsPairingRegistry, identity tunnel.Identity) (*windowsPairingHost, error) {
	deviceName, err := os.Hostname()
	if err != nil || strings.TrimSpace(deviceName) == "" {
		return nil, errors.New("find Windows device name")
	}
	options := []pairing.ServerOption{
		pairing.WithInstaller(installer),
		pairing.WithDisplayName(strings.TrimSpace(deviceName)),
	}
	if registry.store.Path != "" {
		options = append(options,
			pairing.WithSessionGuard(registry.Allow),
			pairing.WithAfterInstall(registry.Commit),
		)
	}
	server, err := pairing.NewServer(
		pairing.ServerIdentity{PrivateKey: append(ed25519.PrivateKey(nil), identity.PrivateKey...)},
		options...,
	)
	if err != nil {
		return nil, err
	}
	return &windowsPairingHost{
		server: server, publisher: discovery.ZeroconfPublisher{}, listen: net.Listen,
		minRetryBackoff: pairingHostMinRetryBackoff, maxRetryBackoff: pairingHostMaxRetryBackoff,
		republishInterval: pairingHostRepublishInterval,
	}, nil
}

func (h *windowsPairingHost) Run(ctx context.Context) {
	tlsConfig, err := h.server.TLSConfig()
	if err != nil {
		return
	}
	instanceID := h.server.InstanceID()
	if instanceID == "" {
		return
	}
	minBackoff, maxBackoff, republishInterval := h.durations()
	backoff := minBackoff
	for ctx.Err() == nil {
		if h.serve(ctx, tlsConfig, instanceID, minBackoff, maxBackoff, republishInterval) {
			return
		}
		if !waitForPairingHostRetry(ctx, backoff) {
			return
		}
		backoff = nextPairingHostBackoff(backoff, maxBackoff)
	}
}

func (h *windowsPairingHost) serve(
	ctx context.Context,
	tlsConfig *tls.Config,
	instanceID string,
	minBackoff, maxBackoff, republishInterval time.Duration,
) bool {
	listen := h.listen
	if listen == nil {
		listen = net.Listen
	}
	listener, err := listen("tcp", windowsPairingListenAddress)
	if err != nil {
		return false
	}
	defer listener.Close()
	tcpAddress, ok := listener.Addr().(*net.TCPAddr)
	if !ok || tcpAddress.Port <= 0 {
		return false
	}
	advertisement, err := discovery.PairingAdvertisement(instanceID, tcpAddress.Port)
	if err != nil {
		return false
	}
	routerConfig, err := tunnel.RoutingTLSConfig(tlsConfig, h.tunnelTLSConfig)
	if err != nil {
		return false
	}
	routed, err := tunnel.RouteTLS(ctx, privatePeerListener{Listener: listener}, routerConfig)
	if err != nil {
		return false
	}
	defer routed.Pairing.Close()
	defer routed.Tunnel.Close()
	server := &http.Server{Handler: h.server, ReadHeaderTimeout: 5 * time.Second}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(routed.Pairing)
	}()
	var tunnelDone chan error
	if h.serveTunnel == nil {
		_ = routed.Tunnel.Close()
	} else {
		tunnelDone = make(chan error, 1)
		go func() { tunnelDone <- h.serveTunnel(ctx, routed.Tunnel) }()
	}
	serveFinished := false
	tunnelFinished := false
	defer func() {
		_ = server.Close()
		_ = routed.Pairing.Close()
		_ = routed.Tunnel.Close()
		if !serveFinished {
			<-serveDone
		}
		if tunnelDone != nil && !tunnelFinished {
			<-tunnelDone
		}
	}()

	backoff := minBackoff
	for {
		publishCtx, cancelPublish := context.WithCancel(ctx)
		registration, publishErr := h.publisher.Publish(publishCtx, advertisement)
		if publishErr != nil {
			cancelPublish()
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				stopTimer(timer)
				return true
			case <-serveDone:
				serveFinished = true
				stopTimer(timer)
				return false
			case <-tunnelDone:
				tunnelFinished = true
				stopTimer(timer)
				return false
			case <-timer.C:
			}
			backoff = nextPairingHostBackoff(backoff, maxBackoff)
			continue
		}

		backoff = minBackoff
		timer := time.NewTimer(republishInterval)
		select {
		case <-ctx.Done():
			stopTimer(timer)
			cancelPublish()
			registration.Shutdown()
			return true
		case <-serveDone:
			serveFinished = true
			stopTimer(timer)
			cancelPublish()
			registration.Shutdown()
			return false
		case <-tunnelDone:
			tunnelFinished = true
			stopTimer(timer)
			cancelPublish()
			registration.Shutdown()
			return false
		case <-timer.C:
			cancelPublish()
			registration.Shutdown()
		}
	}
}

func (h *windowsPairingHost) durations() (time.Duration, time.Duration, time.Duration) {
	minBackoff := h.minRetryBackoff
	if minBackoff <= 0 {
		minBackoff = pairingHostMinRetryBackoff
	}
	maxBackoff := h.maxRetryBackoff
	if maxBackoff < minBackoff {
		maxBackoff = minBackoff
	}
	republishInterval := h.republishInterval
	if republishInterval <= 0 {
		republishInterval = pairingHostRepublishInterval
	}
	return minBackoff, maxBackoff, republishInterval
}

type privatePeerListener struct {
	net.Listener
}

func (l privatePeerListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		address, ok := connection.RemoteAddr().(*net.TCPAddr)
		if ok && address.IP != nil && (address.IP.IsPrivate() || address.IP.IsLoopback()) {
			return connection, nil
		}
		_ = connection.Close()
	}
}

func waitForPairingHostRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer stopTimer(timer)
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextPairingHostBackoff(current, maximum time.Duration) time.Duration {
	if current >= maximum/2 {
		return maximum
	}
	return current * 2
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

type windowsPairingCoordinator struct {
	server    *pairing.Server
	installer pairing.Installer
	registry  *windowsPairingRegistry
}

func (windowsPairingCoordinator) Candidates(context.Context) (localapi.PairCandidatesResult, error) {
	return localapi.PairCandidatesResult{Candidates: []localapi.PairingCandidate{}}, nil
}

func (c windowsPairingCoordinator) Start(context.Context, string) (localapi.PairStartResult, error) {
	descriptor, code, ok := c.server.ActiveSession()
	if !ok {
		return localapi.PairStartResult{}, needsAction("waiting for a Mac pairing request on the private network")
	}
	return localapi.PairStartResult{
		SessionID: descriptor.ID, Code: code, Peer: windowsPairingPeer(descriptor),
		ExpiresAt: descriptor.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}, nil
}

func (c windowsPairingCoordinator) Status(_ context.Context, sessionID string) (localapi.PairingStatusResult, error) {
	descriptor, code, active := c.server.ActiveSession()
	if sessionID == "" && active {
		sessionID = descriptor.ID
	}
	status, ok := c.server.SessionStatus(sessionID)
	if !ok {
		return localapi.PairingStatusResult{}, needsAction("pairing session is not active")
	}
	result := localapi.PairingStatusResult{
		SessionID: status.SessionID, Status: string(status.State), ExpiresAt: status.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
	if active && descriptor.ID == sessionID {
		result.Peer = windowsPairingPeer(descriptor)
		result.Code = code
	} else if status.DeviceID != "" {
		result.Peer = localapi.LifecyclePeer{ID: status.DeviceID, Name: "Mac", OS: "macos"}
		result.Device = &localapi.Device{ID: status.DeviceID, Name: "Mac"}
	}
	return result, nil
}

func (c windowsPairingCoordinator) Observe(ctx context.Context, sessionID string) (localapi.PairingStatusResult, error) {
	return c.Status(ctx, sessionID)
}

func (c windowsPairingCoordinator) Approve(ctx context.Context, sessionID string) (localapi.PairingStatusResult, error) {
	if err := c.server.Approve(sessionID); err != nil {
		return localapi.PairingStatusResult{}, needsAction("pairing request is no longer waiting for approval")
	}
	return c.Status(ctx, sessionID)
}

func (c windowsPairingCoordinator) Reject(ctx context.Context, sessionID string) (localapi.PairingStatusResult, error) {
	descriptor, code, active := c.server.ActiveSession()
	if !active || descriptor.ID != sessionID {
		return localapi.PairingStatusResult{}, needsAction("pairing request is no longer waiting for approval")
	}
	peer := windowsPairingPeer(descriptor)
	if err := c.server.Reject(sessionID); err != nil {
		return localapi.PairingStatusResult{}, needsAction("pairing request is no longer waiting for approval")
	}
	result, err := c.Status(ctx, sessionID)
	if err == nil {
		result.Peer = peer
		result.Code = code
	}
	return result, err
}

func (windowsPairingCoordinator) Cancel(context.Context, string) (localapi.PairingStatusResult, error) {
	return localapi.PairingStatusResult{}, needsAction("only the Mac client can cancel its pairing request")
}

func (c windowsPairingCoordinator) Unpair(ctx context.Context, deviceID string, localOnly bool) error {
	if localOnly {
		return needsAction("local-only forgetting is available only on the Mac client")
	}
	if strings.TrimSpace(deviceID) == "" {
		return needsAction("client device ID is required")
	}
	if err := c.installer.Revoke(ctx, deviceID); err != nil {
		return unavailable("managed pairing revocation failed")
	}
	if c.registry != nil {
		if err := c.registry.Forget(deviceID); err != nil {
			return unavailable("cannot remove trusted device metadata")
		}
	}
	return nil
}

func windowsPairingPeer(descriptor pairing.SessionDescriptor) localapi.LifecyclePeer {
	return localapi.LifecyclePeer{
		ID: pairing.InstanceIDFromPublicKey(descriptor.ClientPublicKey), Name: "Mac", OS: "macos",
	}
}
