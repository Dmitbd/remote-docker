package app

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/credentials"
	"github.com/Dmitbd/remote-docker/internal/diagnostics"
	"github.com/Dmitbd/remote-docker/internal/discovery"
	"github.com/Dmitbd/remote-docker/internal/dockercli"
	"github.com/Dmitbd/remote-docker/internal/systemtransport"
	"github.com/Dmitbd/remote-docker/internal/tunnel"
	"github.com/Dmitbd/remote-docker/internal/windowsbridge"
)

const diagnosticProbeTimeout = 3 * time.Second

const (
	tunnelServerStateWaiting int32 = iota
	tunnelServerStateConnected
	tunnelServerStateBusy
)

type macDiagnosticProbe struct {
	store             config.Store
	secrets           credentials.Store
	tunnelReady       *atomic.Bool
	confirmedPeerBusy *atomic.Bool
	localPortOccupied *atomic.Bool
	dockerCLI         string
	dockerContext     string
	dockerEnv         []string
	sync              productionSyncInspector
	remote            remoteDiagnosticOperations
	dialTunnel        systemtransport.DialContextFunc
}

func (p macDiagnosticProbe) checks() productionDiagnosticsOptions {
	return productionDiagnosticsOptions{
		LANReachability: diagnostics.CheckFunc(p.checkLAN),
		TunnelIdentity:  diagnostics.CheckFunc(p.checkIdentity),
		TunnelSession:   diagnostics.CheckFunc(p.checkSession),
		LocalRelays:     diagnostics.CheckFunc(p.checkLocalRelays),
		DockerChannel:   diagnostics.CheckFunc(p.checkDocker),
		SyncChannel:     diagnostics.CheckFunc(p.checkSync),
		ManagedWSL:      diagnostics.CheckFunc(p.checkWSL),
	}
}

func (p macDiagnosticProbe) savedPeer() (discovery.Peer, error) {
	transport := discoveryPairingTransport{Store: p.store}
	peers := transport.savedPeers()
	if len(peers) != 1 {
		return discovery.Peer{}, errors.New("saved tunnel peer is unavailable")
	}
	return peers[0], nil
}

func (p macDiagnosticProbe) tunnelDialer() systemtransport.DialContextFunc {
	if p.dialTunnel != nil {
		return p.dialTunnel
	}
	return systemtransport.TunnelDialContext()
}

func (p macDiagnosticProbe) checkLAN(ctx context.Context) error {
	peer, err := p.savedPeer()
	if err != nil || len(peer.Addresses) == 0 {
		return diagnostics.NewPublicError(diagnostics.ReasonHostUnreachable)
	}
	probeCtx, cancel := context.WithTimeout(ctx, diagnosticProbeTimeout)
	defer cancel()
	connection, err := p.tunnelDialer()(probeCtx, "tcp", net.JoinHostPort(peer.Addresses[0].String(), strconv.Itoa(tunnel.TunnelPort)))
	if err != nil {
		return diagnostics.NewPublicError(classifyLANDialFailure(err))
	}
	_ = connection.Close()
	return nil
}

func (p macDiagnosticProbe) checkIdentity(ctx context.Context) error {
	if err := p.checkLAN(ctx); err != nil {
		return err
	}
	peer, err := p.savedPeer()
	if err != nil {
		return diagnostics.NewPublicError(diagnostics.ReasonTunnelIdentityMismatch)
	}
	probeCtx, cancel := context.WithTimeout(ctx, diagnosticProbeTimeout)
	defer cancel()
	transport := discoveryPairingTransport{
		Store: p.store, Secrets: p.secrets, TunnelDialContext: p.tunnelDialer(),
	}
	if _, err := transport.verifySavedPeer(probeCtx, peer); err != nil {
		return diagnostics.NewPublicError(diagnostics.ReasonTunnelIdentityMismatch)
	}
	return nil
}

func (p macDiagnosticProbe) checkSession(ctx context.Context) error {
	connected := p.tunnelReady != nil && p.tunnelReady.Load()
	if connected {
		return nil
	}
	if p.confirmedPeerBusy != nil && p.confirmedPeerBusy.Load() {
		return tunnelSessionStateError(false, true)
	}
	if err := p.checkIdentity(ctx); err != nil {
		return err
	}
	return tunnelSessionStateError(false, false)
}

func updateMacTunnelDiagnosticState(
	ready, confirmedBusy, localPortOccupied *atomic.Bool,
	state tunnel.ClientState,
	err error,
) {
	ready.Store(state == tunnel.ClientConnected)
	switch {
	case state == tunnel.ClientConnected:
		confirmedBusy.Store(false)
		localPortOccupied.Store(false)
	case errors.Is(err, tunnel.ErrPeerBusy):
		confirmedBusy.Store(true)
		localPortOccupied.Store(false)
	case errors.Is(err, tunnel.ErrLocalPortOccupied):
		confirmedBusy.Store(false)
		localPortOccupied.Store(true)
	case err != nil:
		confirmedBusy.Store(false)
		localPortOccupied.Store(false)
	}
}

func tunnelSessionStateError(connected, confirmedBusy bool) error {
	if connected {
		return nil
	}
	if confirmedBusy {
		return diagnostics.NewPublicError(diagnostics.ReasonPeerBusy)
	}
	return diagnostics.NewPublicError(diagnostics.ReasonRemoteConnectionNotReady)
}

func (p macDiagnosticProbe) checkLocalRelays(ctx context.Context) error {
	if p.localPortOccupied != nil && p.localPortOccupied.Load() {
		return diagnostics.NewPublicError(diagnostics.ReasonLocalPortOccupied)
	}
	if p.tunnelReady == nil || !p.tunnelReady.Load() {
		return p.checkSession(ctx)
	}
	for _, port := range []int{tunnel.SyncRelayPort, tunnel.DockerRelayPort, tunnel.ControlRelayPort, tunnel.MetricsRelayPort} {
		probeCtx, cancel := context.WithTimeout(ctx, diagnosticProbeTimeout)
		connection, err := (&net.Dialer{}).DialContext(probeCtx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		cancel()
		if err != nil {
			return diagnostics.NewPublicError(diagnostics.ReasonLocalPortOccupied)
		}
		_ = connection.Close()
	}
	return nil
}

func (p macDiagnosticProbe) checkDocker(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, diagnosticProbeTimeout)
	defer cancel()
	if err := (dockercli.Runner{}).Run(probeCtx, dockercli.Invocation{
		Binary: p.dockerCLI, Args: []string{"--context", p.dockerContext, "version", "--format", "{{.Server.Version}}"},
		Env: p.dockerEnv, Stdout: io.Discard, Stderr: io.Discard,
	}); err != nil {
		return errors.New("Docker tunnel channel probe failed")
	}
	return nil
}

func (p macDiagnosticProbe) checkSync(ctx context.Context) error {
	cfg, err := loadAgentConfig(p.store)
	if err != nil {
		return errors.New("sync tunnel channel probe failed")
	}
	probeCtx, cancel := context.WithTimeout(ctx, diagnosticProbeTimeout)
	defer cancel()
	connected, err := p.sync.Connected(probeCtx, cfg)
	if err != nil || !connected {
		return errors.New("sync tunnel channel probe failed")
	}
	return nil
}

func (p macDiagnosticProbe) checkWSL(ctx context.Context) error {
	if p.remote == nil {
		return diagnostics.ErrCheckUnavailable
	}
	probeCtx, cancel := context.WithTimeout(ctx, diagnosticProbeTimeout)
	defer cancel()
	status, err := p.remote.Observe(probeCtx)
	if err != nil || !status.WSLRunning {
		return diagnostics.NewPublicError(diagnostics.ReasonWSLUnavailable)
	}
	return nil
}

func classifyLANDialFailure(err error) diagnostics.Reason {
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return diagnostics.ReasonLANBlocked
	}
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.ECONNREFUSED) {
		return diagnostics.ReasonLANBlocked
	}
	return diagnostics.ReasonHostUnreachable
}

type windowsDiagnosticProbe struct {
	secrets     credentials.Store
	operations  windowsDiagnosticOperations
	serverState *atomic.Int32
}

func (p windowsDiagnosticProbe) checks() productionDiagnosticsOptions {
	observe := func(selectValue func(windowsbridge.ManagedWSLStatus) bool, reason diagnostics.Reason) diagnostics.Check {
		return diagnostics.CheckFunc(func(ctx context.Context) error {
			if p.operations == nil {
				return diagnostics.ErrCheckUnavailable
			}
			status, err := p.operations.Observe(ctx)
			if err != nil || !selectValue(status) {
				return diagnostics.NewPublicError(reason)
			}
			return nil
		})
	}
	return productionDiagnosticsOptions{
		LANReachability: diagnostics.CheckFunc(func(context.Context) error {
			if p.serverState == nil {
				return diagnostics.NewPublicError(diagnostics.ReasonRemoteConnectionNotReady)
			}
			state := p.serverState.Load()
			if state == tunnelServerStateConnected || state == tunnelServerStateBusy {
				return nil
			}
			return diagnostics.NewPublicError(diagnostics.ReasonRemoteConnectionNotReady)
		}),
		TunnelIdentity: diagnostics.CheckFunc(func(context.Context) error {
			encoded, err := p.secrets.Get(tunnel.WindowsIdentityOwner, tunnel.IdentityCredential)
			if err != nil {
				return diagnostics.NewPublicError(diagnostics.ReasonTunnelIdentityMismatch)
			}
			defer clearSecret(encoded)
			if _, err := tunnel.IdentityFromPKCS8(encoded); err != nil {
				return diagnostics.NewPublicError(diagnostics.ReasonTunnelIdentityMismatch)
			}
			return nil
		}),
		TunnelSession: diagnostics.CheckFunc(func(context.Context) error {
			if p.serverState == nil {
				return tunnelSessionStateError(false, false)
			}
			state := p.serverState.Load()
			return tunnelSessionStateError(state == tunnelServerStateConnected, state == tunnelServerStateBusy)
		}),
		LocalRelays:   diagnostics.CheckFunc(func(context.Context) error { return nil }),
		DockerChannel: observe(func(status windowsbridge.ManagedWSLStatus) bool { return status.DockerSocket }, diagnostics.ReasonWSLUnavailable),
		SyncChannel:   observe(func(status windowsbridge.ManagedWSLStatus) bool { return status.SyncthingService }, diagnostics.ReasonWSLUnavailable),
		ManagedWSL:    observe(func(status windowsbridge.ManagedWSLStatus) bool { return status.Running }, diagnostics.ReasonWSLUnavailable),
	}
}
