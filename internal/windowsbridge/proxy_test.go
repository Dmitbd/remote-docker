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
)

func TestProxyResolvesWSLAddressForEveryConnection(t *testing.T) {
	target := startEchoServer(t)
	resolver := &sequenceResolver{addresses: []net.IP{
		net.ParseIP("172.30.1.10"),
		net.ParseIP("172.30.1.11"),
	}}
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

	wantAddresses := []string{"172.30.1.10:22", "172.30.1.11:22"}
	if got := dialer.addresses(); !reflect.DeepEqual(got, wantAddresses) {
		t.Fatalf("dial addresses = %#v, want %#v", got, wantAddresses)
	}
}

func TestProxyRejectsUnsupportedServices(t *testing.T) {
	_, err := NewProxy(Service("docker-api"), &sequenceResolver{}, staticProfile(true), &net.Dialer{})
	if !errors.Is(err, ErrUnsupportedService) {
		t.Fatalf("NewProxy() error = %v, want ErrUnsupportedService", err)
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
	if r.next >= len(r.addresses) {
		return nil, errors.New("no WSL address")
	}
	address := r.addresses[r.next]
	r.next++
	return address, nil
}

type redirectDialer struct {
	mu       sync.Mutex
	target   string
	recorded []string
}

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
