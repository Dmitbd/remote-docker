package windowsbridge

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestPowerShellPrivateNetworkUsesFixedCommandAndFiltersAddresses(t *testing.T) {
	runner := &recordingOutputRunner{outputs: [][]byte{[]byte(`["192.168.1.68","203.0.113.8","fe80::1"]`)}}
	provider := PowerShellPrivateNetwork{Runner: runner}

	addresses, err := provider.Addresses(context.Background())
	if err != nil {
		t.Fatalf("Addresses() error = %v", err)
	}
	if got, want := ipStrings(addresses), []string{"192.168.1.68"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses = %#v, want %#v", got, want)
	}
	wantCommand := []string{
		"powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", privateIPv4PowerShell,
	}
	if !reflect.DeepEqual(runner.calls, [][]string{wantCommand}) {
		t.Fatalf("commands = %#v, want %#v", runner.calls, [][]string{wantCommand})
	}
}

func TestPrivateNetworkDecodesPowerShellUTF16WithoutBOM(t *testing.T) {
	text := `[` + `"192.168.1.68"` + `]`
	encoded := make([]byte, 0, len(text)*2)
	for _, character := range []byte(text) {
		encoded = append(encoded, character, 0)
	}
	addresses, err := decodePrivateIPv4(encoded)
	if err != nil || !reflect.DeepEqual(ipStrings(addresses), []string{"192.168.1.68"}) {
		t.Fatalf("decodePrivateIPv4(UTF-16LE) = %#v, %v", ipStrings(addresses), err)
	}
}

func TestHostDoesNotOpenLegacyLANBridgeListeners(t *testing.T) {
	provider := staticAddressProvider{net.ParseIP("192.168.1.68")}
	listeners := &recordingListenerFactory{}
	host, err := NewHost(provider, &sequenceResolver{}, &net.Dialer{})
	if err != nil {
		t.Fatalf("NewHost() error = %v", err)
	}
	host.Listen = listeners.Listen

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx, time.Millisecond) }()

	time.Sleep(10 * time.Millisecond)
	if got := listeners.Addresses(); len(got) != 0 {
		t.Fatalf("legacy LAN listeners were opened: %#v", got)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
}

func TestHostDoesNotProbeLANAddressesWhileTunnelOwnsNetwork(t *testing.T) {
	provider := &sequenceAddressProvider{results: []addressResult{
		{err: errors.New("network profile unavailable")},
		{addresses: []net.IP{net.ParseIP("192.168.1.68")}},
	}}
	listeners := &recordingListenerFactory{}
	host, err := NewHost(provider, &sequenceResolver{}, &net.Dialer{})
	if err != nil {
		t.Fatal(err)
	}
	host.Listen = listeners.Listen

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx, time.Millisecond) }()

	time.Sleep(10 * time.Millisecond)
	if len(listeners.Addresses()) != 0 || provider.Calls() != 0 {
		t.Fatalf("listeners=%#v provider calls=%d, want no raw LAN bridge activity", listeners.Addresses(), provider.Calls())
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
}

func ipStrings(addresses []net.IP) []string {
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		result = append(result, address.String())
	}
	return result
}

type staticAddressProvider []net.IP

func (p staticAddressProvider) Addresses(context.Context) ([]net.IP, error) {
	return append([]net.IP(nil), p...), nil
}

type addressResult struct {
	addresses []net.IP
	err       error
}

type sequenceAddressProvider struct {
	mu      sync.Mutex
	results []addressResult
	calls   int
}

func (p *sequenceAddressProvider) Addresses(context.Context) ([]net.IP, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if len(p.results) == 0 {
		return nil, errors.New("no address result")
	}
	result := p.results[0]
	if len(p.results) > 1 {
		p.results = p.results[1:]
	}
	return append([]net.IP(nil), result.addresses...), result.err
}

func (p *sequenceAddressProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type recordingListenerFactory struct {
	mu        sync.Mutex
	addresses []string
	listeners []*blockingListener
}

func (f *recordingListenerFactory) Listen(network, address string) (net.Listener, error) {
	if network != "tcp" {
		return nil, errors.New("unexpected network")
	}
	tcpAddress, err := net.ResolveTCPAddr(network, address)
	if err != nil {
		return nil, err
	}
	listener := &blockingListener{address: tcpAddress, closed: make(chan struct{})}
	f.mu.Lock()
	f.addresses = append(f.addresses, address)
	f.listeners = append(f.listeners, listener)
	f.mu.Unlock()
	return listener, nil
}

func (f *recordingListenerFactory) Addresses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := append([]string(nil), f.addresses...)
	if len(result) == 2 && result[0] > result[1] {
		result[0], result[1] = result[1], result[0]
	}
	return result
}

type blockingListener struct {
	address net.Addr
	closed  chan struct{}
	once    sync.Once
}

func (l *blockingListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *blockingListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *blockingListener) Addr() net.Addr { return l.address }
