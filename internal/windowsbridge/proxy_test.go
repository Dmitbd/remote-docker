package windowsbridge

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/tunnel"
)

func TestProxyPrefersIPv6LoopbackWithoutResolvingWSLAddress(t *testing.T) {
	target := startEchoServer(t)
	resolver := &sequenceResolver{}
	dialer := &redirectDialer{target: target.Addr().String()}
	proxy, err := NewProxy(ServiceSSH, resolver, staticProfile(true), dialer)
	if err != nil {
		t.Fatalf("NewProxy() error = %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(ctx, listener) }()

	for _, payload := range []string{"first", "second"} {
		connection, dialErr := net.Dial("tcp", listener.Addr().String())
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		if _, writeErr := io.WriteString(connection, payload); writeErr != nil {
			t.Fatal(writeErr)
		}
		response := make([]byte, len(payload))
		if _, readErr := io.ReadFull(connection, response); readErr != nil {
			t.Fatal(readErr)
		}
		if string(response) != payload {
			t.Fatalf("response = %q, want %q", response, payload)
		}
		_ = connection.Close()
	}

	cancel()
	_ = listener.Close()
	select {
	case serveErr := <-done:
		if serveErr != nil && !errors.Is(serveErr, context.Canceled) {
			t.Fatalf("Serve() error = %v", serveErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve() did not stop")
	}

	wantAddresses := []string{"[::1]:22", "[::1]:22"}
	if got := dialer.addresses(); !reflect.DeepEqual(got, wantAddresses) {
		t.Fatalf("dial addresses = %#v, want %#v", got, wantAddresses)
	}
	if resolver.calls() != 0 {
		t.Fatalf("WSL resolver calls = %d, want 0 while loopback works", resolver.calls())
	}
}

func TestProxyFallsBackFromLoopbackToFreshPrivateWSLAddress(t *testing.T) {
	resolver := &sequenceResolver{addresses: []net.IP{net.ParseIP("172.30.1.10")}}
	dialer := &scriptedDialer{
		failures: 2,
		connection: &addressedConn{
			local:  &net.TCPAddr{IP: net.ParseIP("172.30.1.1"), Port: 40000},
			remote: &net.TCPAddr{IP: net.ParseIP("172.30.1.10"), Port: 22},
		},
	}
	proxy, err := NewProxy(ServiceSSH, resolver, staticProfile(true), dialer)
	if err != nil {
		t.Fatal(err)
	}

	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		proxy.forward(context.Background(), server)
		close(done)
	}()
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forward() did not stop")
	}

	want := []string{"[::1]:22", "127.0.0.1:22", "172.30.1.10:22"}
	if got := dialer.addresses(); !reflect.DeepEqual(got, want) {
		t.Fatalf("dial addresses = %#v, want %#v", got, want)
	}
	if resolver.calls() != 1 {
		t.Fatalf("WSL resolver calls = %d, want 1 fallback lookup", resolver.calls())
	}
}

func TestProxyBoundsEveryUpstreamDialAttempt(t *testing.T) {
	dialer := &deadlineDialer{}
	proxy, err := NewProxy(ServiceSyncthing, &sequenceResolver{addresses: []net.IP{net.ParseIP("172.30.1.10")}}, staticProfile(true), dialer)
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	proxy.forward(context.Background(), server)
	_ = client.Close()
	if dialer.calls != 3 || !dialer.allBounded {
		t.Fatalf("dial attempts = %d, all bounded = %v; want 3 bounded attempts", dialer.calls, dialer.allBounded)
	}
}

func TestProxyRejectsUnsupportedServices(t *testing.T) {
	_, err := NewProxy(Service("docker-api"), &sequenceResolver{}, staticProfile(true), &net.Dialer{})
	if !errors.Is(err, ErrUnsupportedService) {
		t.Fatalf("NewProxy() error = %v, want ErrUnsupportedService", err)
	}
}

func TestTunnelServiceDialerMapsOnlyFourKindsToFixedWSLServices(t *testing.T) {
	for _, test := range []struct {
		kind tunnel.StreamKind
		port string
	}{
		{tunnel.StreamDockerSSH, "22"},
		{tunnel.StreamControl, "22"},
		{tunnel.StreamMetrics, "22"},
		{tunnel.StreamWorkspaceSync, "22000"},
	} {
		t.Run(test.kind.String(), func(t *testing.T) {
			dialer := &scriptedDialer{failures: 3}
			serviceDialer := &ServiceDialer{
				Resolver: &sequenceResolver{addresses: []net.IP{net.ParseIP("172.30.1.10")}}, Dialer: dialer,
			}
			_, _ = serviceDialer.DialService(context.Background(), test.kind)
			want := []string{"[::1]:" + test.port, "127.0.0.1:" + test.port, "172.30.1.10:" + test.port}
			if got := dialer.addresses(); !reflect.DeepEqual(got, want) {
				t.Fatalf("DialService(%s) addresses = %#v, want %#v", test.kind, got, want)
			}
		})
	}
	dialer := &recordingDialer{}
	serviceDialer := &ServiceDialer{Resolver: &sequenceResolver{}, Dialer: dialer}
	if _, err := serviceDialer.DialService(context.Background(), 99); !errors.Is(err, ErrUnsupportedService) {
		t.Fatalf("DialService(unknown) error = %v", err)
	}
	if len(dialer.addresses()) != 0 {
		t.Fatal("unknown tunnel kind dialed WSL")
	}
}

func TestProxyDoesNotAcceptOnPublicProfile(t *testing.T) {
	proxy, err := NewProxy(ServiceSyncthing, &sequenceResolver{}, staticProfile(false), &net.Dialer{})
	if err != nil {
		t.Fatal(err)
	}
	listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatal(listenErr)
	}
	defer listener.Close()

	err = proxy.Serve(context.Background(), listener)
	if !errors.Is(err, ErrPublicNetwork) {
		t.Fatalf("Serve() error = %v, want ErrPublicNetwork", err)
	}
}

func TestProxyRejectsWildcardListener(t *testing.T) {
	proxy, err := NewProxy(ServiceSSH, &sequenceResolver{}, staticProfile(true), &net.Dialer{})
	if err != nil {
		t.Fatal(err)
	}
	listener := &stubListener{address: &net.TCPAddr{IP: net.IPv4zero, Port: 49222}}
	if serveErr := proxy.Serve(context.Background(), listener); !errors.Is(serveErr, ErrUnsafeListenAddress) {
		t.Fatalf("Serve() error = %v, want ErrUnsafeListenAddress", serveErr)
	}
}

func TestProxyRejectsPublicRemotePeerBeforeDialingWSL(t *testing.T) {
	dialer := &recordingDialer{}
	listener := &queuedListener{
		address: &net.TCPAddr{IP: net.ParseIP("192.168.1.68"), Port: 49222},
		connections: []net.Conn{&addressedConn{
			local:  &net.TCPAddr{IP: net.ParseIP("192.168.1.68"), Port: 49222},
			remote: &net.TCPAddr{IP: net.ParseIP("203.0.113.8"), Port: 50000},
		}},
	}
	proxy, err := NewProxy(ServiceSSH, &sequenceResolver{}, staticProfile(true), dialer)
	if err != nil {
		t.Fatal(err)
	}
	if err := proxy.Serve(context.Background(), listener); err == nil {
		t.Fatal("Serve() error = nil, want listener exhaustion after rejecting public peer")
	}
	if len(dialer.addresses()) != 0 {
		t.Fatalf("WSL dial addresses = %#v, want none", dialer.addresses())
	}
}

func TestProxyStopsAcceptingWhenProfileBecomesPublic(t *testing.T) {
	profiles := &sequenceProfile{values: []bool{true, false}}
	proxy, err := NewProxy(ServiceSSH, &sequenceResolver{}, profiles, &net.Dialer{})
	if err != nil {
		t.Fatal(err)
	}
	listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatal(listenErr)
	}
	defer listener.Close()

	done := make(chan error, 1)
	go func() { done <- proxy.Serve(context.Background(), listener) }()
	select {
	case serveErr := <-done:
		if !errors.Is(serveErr, ErrPublicNetwork) {
			t.Fatalf("Serve() error = %v, want ErrPublicNetwork", serveErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve() kept accepting after profile became public")
	}
}

func TestWSLResolverRunsExactCommandForEveryLookup(t *testing.T) {
	runner := &recordingOutputRunner{outputs: [][]byte{
		[]byte("172.30.1.10  fe80::1\n"),
		[]byte("172.30.1.11\n"),
	}}
	resolver := WSLResolver{Distro: "remote-docker", Runner: runner}

	first, err := resolver.WSLAddress(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolver.WSLAddress(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.String() != "172.30.1.10" || second.String() != "172.30.1.11" {
		t.Fatalf("addresses = %s, %s", first, second)
	}
	want := [][]string{
		{"wsl.exe", "-d", "remote-docker", "--", "hostname", "-I"},
		{"wsl.exe", "-d", "remote-docker", "--", "hostname", "-I"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("commands = %#v, want %#v", runner.calls, want)
	}
}

func TestWSLResolverRejectsNonPrivateAddresses(t *testing.T) {
	runner := &recordingOutputRunner{outputs: [][]byte{[]byte("203.0.113.8\n")}}
	resolver := WSLResolver{Distro: "remote-docker", Runner: runner}
	if _, err := resolver.WSLAddress(context.Background()); !errors.Is(err, ErrNoPrivateWSLAddress) {
		t.Fatalf("WSLAddress() error = %v, want ErrNoPrivateWSLAddress", err)
	}
}

type sequenceResolver struct {
	mu        sync.Mutex
	addresses []net.IP
	next      int
	called    int
}

type stubListener struct {
	address net.Addr
}

func (l *stubListener) Accept() (net.Conn, error) { return nil, errors.New("unexpected accept") }
func (l *stubListener) Close() error              { return nil }
func (l *stubListener) Addr() net.Addr            { return l.address }

func (r *sequenceResolver) WSLAddress(context.Context) (net.IP, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.called++
	if r.next >= len(r.addresses) {
		return nil, errors.New("no WSL address")
	}
	address := r.addresses[r.next]
	r.next++
	return address, nil
}

func (r *sequenceResolver) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.called
}

type redirectDialer struct {
	mu       sync.Mutex
	target   string
	recorded []string
}

type recordingDialer struct {
	mu       sync.Mutex
	recorded []string
}

type scriptedDialer struct {
	mu         sync.Mutex
	failures   int
	connection net.Conn
	recorded   []string
}

func (d *scriptedDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.recorded = append(d.recorded, address)
	if d.failures > 0 {
		d.failures--
		return nil, errors.New("dial failed")
	}
	return d.connection, nil
}

func (d *scriptedDialer) addresses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.recorded...)
}

type deadlineDialer struct {
	calls      int
	allBounded bool
}

func (d *deadlineDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	d.calls++
	_, bounded := ctx.Deadline()
	if d.calls == 1 {
		d.allBounded = bounded
	} else {
		d.allBounded = d.allBounded && bounded
	}
	return nil, errors.New("dial failed")
}

func (d *recordingDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	d.mu.Lock()
	d.recorded = append(d.recorded, "called")
	d.mu.Unlock()
	return nil, errors.New("unexpected dial")
}

func (d *recordingDialer) addresses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.recorded...)
}

type queuedListener struct {
	address     net.Addr
	connections []net.Conn
}

func (l *queuedListener) Accept() (net.Conn, error) {
	if len(l.connections) == 0 {
		return nil, errors.New("listener exhausted")
	}
	connection := l.connections[0]
	l.connections = l.connections[1:]
	return connection, nil
}

func (*queuedListener) Close() error     { return nil }
func (l *queuedListener) Addr() net.Addr { return l.address }

type addressedConn struct {
	local  net.Addr
	remote net.Addr
}

func (*addressedConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (*addressedConn) Write(value []byte) (int, error)  { return len(value), nil }
func (*addressedConn) Close() error                     { return nil }
func (c *addressedConn) LocalAddr() net.Addr            { return c.local }
func (c *addressedConn) RemoteAddr() net.Addr           { return c.remote }
func (*addressedConn) SetDeadline(time.Time) error      { return nil }
func (*addressedConn) SetReadDeadline(time.Time) error  { return nil }
func (*addressedConn) SetWriteDeadline(time.Time) error { return nil }

type recordingOutputRunner struct {
	outputs [][]byte
	calls   [][]string
}

func (r *recordingOutputRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if len(r.outputs) == 0 {
		return nil, errors.New("no command output")
	}
	output := r.outputs[0]
	r.outputs = r.outputs[1:]
	return output, nil
}

func (d *redirectDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.recorded = append(d.recorded, address)
	d.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, network, d.target)
}

func (d *redirectDialer) addresses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.recorded...)
}

type staticProfile bool

func (p staticProfile) Private(context.Context) (bool, error) { return bool(p), nil }

type sequenceProfile struct {
	mu     sync.Mutex
	values []bool
	next   int
}

func (p *sequenceProfile) Private(context.Context) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.next >= len(p.values) {
		return p.values[len(p.values)-1], nil
	}
	value := p.values[p.next]
	p.next++
	return value, nil
}

func startEchoServer(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer connection.Close()
				_, _ = io.Copy(connection, connection)
			}()
		}
	}()
	return listener
}
