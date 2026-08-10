package windowsbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Dmitbd/remote-docker/internal/tunnel"
)

// Service identifies one of the only two protocols the Windows bridge exposes.
type Service string

const (
	ServiceSSH       Service = "ssh"
	ServiceSyncthing Service = "syncthing"
)

var (
	ErrUnsupportedService  = errors.New("unsupported bridge service")
	ErrPublicNetwork       = errors.New("bridge disabled on a public network profile")
	ErrResolverUnavailable = errors.New("WSL address resolver is unavailable")
	ErrProfileUnavailable  = errors.New("network profile provider is unavailable")
	ErrDialerUnavailable   = errors.New("bridge dialer is unavailable")
	ErrNoPrivateWSLAddress = errors.New("WSL did not report a private IP address")
	ErrUnsafeListenAddress = errors.New("bridge must bind an explicit private interface address")
)

const (
	profilePollInterval      = 100 * time.Millisecond
	upstreamDialAttemptLimit = 1500 * time.Millisecond
)

// AddressResolver returns the current WSL2 address. It is called per connection.
type AddressResolver interface {
	WSLAddress(context.Context) (net.IP, error)
}

// ProfileProvider reports whether the active Windows network is Private.
type ProfileProvider interface {
	Private(context.Context) (bool, error)
}

// Dialer opens the internal connection to WSL.
type Dialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

// OutputRunner executes the read-only address lookup used by WSLResolver.
type OutputRunner interface {
	Output(context.Context, string, ...string) ([]byte, error)
}

// WSLResolver asks WSL for the current distro address on every lookup.
type WSLResolver struct {
	Distro string
	Runner OutputRunner
}

// WSLAddress returns the first private address from `hostname -I`.
func (r WSLResolver) WSLAddress(ctx context.Context) (net.IP, error) {
	if strings.TrimSpace(r.Distro) == "" {
		return nil, errors.New("managed WSL distribution name is empty")
	}
	runner := r.Runner
	if runner == nil {
		runner = commandOutputRunner{}
	}
	output, err := runner.Output(ctx, "wsl.exe", "-d", r.Distro, "--", "hostname", "-I")
	if err != nil {
		return nil, fmt.Errorf("resolve managed WSL address: %w", err)
	}
	for _, field := range strings.Fields(string(output)) {
		address := net.ParseIP(field)
		if address != nil && address.IsPrivate() {
			return address, nil
		}
	}
	return nil, ErrNoPrivateWSLAddress
}

type commandOutputRunner struct{}

func (commandOutputRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	configureHiddenProcess(command)
	return command.Output()
}

// ServiceDialer maps the four authenticated tunnel streams to two fixed WSL
// loopback services. It cannot accept a caller-provided destination or port.
type ServiceDialer struct {
	Resolver AddressResolver
	Dialer   Dialer
}

func NewProductionServiceDialer() *ServiceDialer {
	return &ServiceDialer{Resolver: WSLResolver{Distro: managedDistroName}, Dialer: &net.Dialer{}}
}

func (d *ServiceDialer) DialService(ctx context.Context, kind tunnel.StreamKind) (net.Conn, error) {
	if d == nil || d.Resolver == nil || d.Dialer == nil {
		return nil, ErrDialerUnavailable
	}
	port := 0
	switch kind {
	case tunnel.StreamDockerSSH, tunnel.StreamControl, tunnel.StreamMetrics:
		port = 22
	case tunnel.StreamWorkspaceSync:
		port = 22000
	default:
		return nil, ErrUnsupportedService
	}
	proxy := &Proxy{targetPort: port, resolver: d.Resolver, dialer: d.Dialer}
	return proxy.dialUpstream(ctx)
}

// Proxy forwards one fixed, authenticated service without inspecting its bytes.
type Proxy struct {
	targetPort int
	resolver   AddressResolver
	profiles   ProfileProvider
	dialer     Dialer
}

// NewProxy permits SSH and Syncthing only; it cannot be configured as a generic
// Docker API or arbitrary-port proxy.
func NewProxy(service Service, resolver AddressResolver, profiles ProfileProvider, dialer Dialer) (*Proxy, error) {
	port := 0
	switch service {
	case ServiceSSH:
		port = 22
	case ServiceSyncthing:
		port = 22000
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedService, service)
	}
	if resolver == nil {
		return nil, ErrResolverUnavailable
	}
	if profiles == nil {
		return nil, ErrProfileUnavailable
	}
	if dialer == nil {
		return nil, ErrDialerUnavailable
	}
	return &Proxy{targetPort: port, resolver: resolver, profiles: profiles, dialer: dialer}, nil
}

// Serve accepts while the network profile is Private. A TCP listener deadline
// makes profile changes observable even when no client is connecting.
func (p *Proxy) Serve(ctx context.Context, listener net.Listener) error {
	if listener == nil {
		return errors.New("bridge listener is unavailable")
	}
	listenAddress, ok := listener.Addr().(*net.TCPAddr)
	if !ok || listenAddress.IP == nil || listenAddress.IP.IsUnspecified() ||
		(!listenAddress.IP.IsPrivate() && !listenAddress.IP.IsLoopback()) {
		return ErrUnsafeListenAddress
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.requirePrivate(ctx); err != nil {
			return err
		}

		if deadlineListener, ok := listener.(interface{ SetDeadline(time.Time) error }); ok {
			if err := deadlineListener.SetDeadline(time.Now().Add(profilePollInterval)); err != nil {
				return fmt.Errorf("set bridge accept deadline: %w", err)
			}
		}
		connection, err := listener.Accept()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if networkErr, ok := err.(net.Error); ok && networkErr.Timeout() {
				continue
			}
			return fmt.Errorf("accept bridge connection: %w", err)
		}
		remoteAddress, ok := connection.RemoteAddr().(*net.TCPAddr)
		if !ok || remoteAddress.IP == nil ||
			(!remoteAddress.IP.IsPrivate() && !remoteAddress.IP.IsLoopback()) {
			_ = connection.Close()
			continue
		}

		if err := p.requirePrivate(ctx); err != nil {
			_ = connection.Close()
			return err
		}
		go p.forward(ctx, connection)
	}
}

func (p *Proxy) requirePrivate(ctx context.Context) error {
	private, err := p.profiles.Private(ctx)
	if err != nil {
		return fmt.Errorf("read network profile: %w", err)
	}
	if !private {
		return ErrPublicNetwork
	}
	return nil
}

func (p *Proxy) forward(ctx context.Context, inbound net.Conn) {
	defer inbound.Close()

	outbound, err := p.dialUpstream(ctx)
	if err != nil {
		return
	}
	defer outbound.Close()

	done := make(chan struct{}, 2)
	copyStream := func(destination net.Conn, source net.Conn) {
		_, _ = io.Copy(destination, source)
		if closeWriter, ok := destination.(interface{ CloseWrite() error }); ok {
			_ = closeWriter.CloseWrite()
		}
		done <- struct{}{}
	}
	go copyStream(outbound, inbound)
	go copyStream(inbound, outbound)

	select {
	case <-ctx.Done():
		_ = inbound.Close()
		_ = outbound.Close()
		<-done
		<-done
	case <-done:
		<-done
	}
}

// dialUpstream prefers Windows' WSL loopback relay because traffic to the
// distro's private subnet can be intercepted by VPN software. The private WSL
// address remains a compatibility fallback for systems without loopback
// forwarding. Every attempt is bounded independently.
func (p *Proxy) dialUpstream(ctx context.Context) (net.Conn, error) {
	port := strconv.Itoa(p.targetPort)
	loopbackTargets := []string{
		net.JoinHostPort("::1", port),
		net.JoinHostPort("127.0.0.1", port),
	}
	var lastErr error
	for _, target := range loopbackTargets {
		connection, err := p.dialBounded(ctx, target)
		if err == nil {
			return connection, nil
		}
		lastErr = err
	}

	address, err := p.resolver.WSLAddress(ctx)
	if err != nil || address == nil {
		if err != nil {
			return nil, err
		}
		return nil, lastErr
	}
	connection, err := p.dialBounded(ctx, net.JoinHostPort(address.String(), port))
	if err != nil {
		return nil, err
	}
	return connection, nil
}

func (p *Proxy) dialBounded(ctx context.Context, target string) (net.Conn, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, upstreamDialAttemptLimit)
	defer cancel()
	return p.dialer.DialContext(attemptCtx, "tcp", target)
}
