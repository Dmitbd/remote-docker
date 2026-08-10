package transportlab_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/pairing"
	"github.com/Dmitbd/remote-docker/internal/tunnel"
)

var labTime = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

func TestSecureTransportLabPairingStreamsReconnectLimitAndCleanup(t *testing.T) {
	macIdentity := deterministicIdentity(1)
	windowsIdentity := deterministicIdentity(2)
	descriptor := pairing.SessionDescriptor{
		ID: "000102030405060708090a0b0c0d0e0f", Nonce: bytes.Repeat([]byte{7}, 32),
		ServerPublicKey: windowsIdentity.PublicKey, ClientPublicKey: macIdentity.PublicKey,
		ExpiresAt: labTime.Add(time.Minute),
	}
	firstCode, err := pairing.Code(descriptor)
	if err != nil {
		t.Fatalf("pairing Code() error = %v", err)
	}
	secondCode, _ := pairing.Code(descriptor)
	if firstCode != secondCode || len(firstCode) != 6 || pairing.InstanceIDFromPublicKey(windowsIdentity.PublicKey) == "" {
		t.Fatalf("deterministic pairing descriptor produced %q and %q", firstCode, secondCode)
	}

	lab := newTunnelLab(t, macIdentity, windowsIdentity)
	client := lab.dial(t, macIdentity, windowsIdentity.PublicKey)
	defer client.Close()
	lab.waitState(t, tunnel.ServerConnected)

	kinds := []tunnel.StreamKind{
		tunnel.StreamDockerSSH, tunnel.StreamWorkspaceSync, tunnel.StreamControl, tunnel.StreamMetrics,
	}
	var wait sync.WaitGroup
	for _, kind := range kinds {
		kind := kind
		wait.Add(1)
		go func() {
			defer wait.Done()
			assertEcho(t, client, kind, []byte("payload-"+kind.String()))
		}()
	}
	wait.Wait()
	if got := lab.dialer.kinds(); !sameKindCounts(got, kinds) {
		t.Fatalf("routed stream kinds = %v, want all four fixed kinds", got)
	}

	second := lab.dial(t, macIdentity, windowsIdentity.PublicKey)
	select {
	case <-second.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("second authenticated client was not rejected")
	}
	_ = second.Close()
	lab.waitState(t, tunnel.ServerBusy)

	_ = client.Close()
	lab.waitState(t, tunnel.ServerWaiting)
	reconnected := lab.dial(t, macIdentity, windowsIdentity.PublicKey)
	lab.waitState(t, tunnel.ServerConnected)
	assertEcho(t, reconnected, tunnel.StreamControl, []byte("after-reconnect"))
	_ = reconnected.Close()

	wrongIdentity := deterministicIdentity(3)
	wrongSession, err := lab.tryDial(wrongIdentity, windowsIdentity.PublicKey)
	if err == nil {
		defer wrongSession.Close()
		select {
		case <-wrongSession.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("unpaired client identity remained accepted by the server")
		}
	}
	lab.close(t)
	if active := lab.active.Load(); active != 0 {
		t.Fatalf("transport lab retained %d owned listeners, connections or goroutines", active)
	}
}

type tunnelLab struct {
	ctx       context.Context
	cancel    context.CancelFunc
	listener  net.Listener
	serverTLS *tls.Config
	dialer    *echoServiceDialer
	states    chan tunnel.ServerState
	done      chan error
	active    atomic.Int32
	wait      sync.WaitGroup
	closeOnce sync.Once
}

func newTunnelLab(t *testing.T, macIdentity, windowsIdentity tunnel.Identity) *tunnelLab {
	t.Helper()
	serverTLS, err := tunnel.ServerTLSConfig(windowsIdentity, func(key ed25519.PublicKey) bool {
		return bytes.Equal(key, macIdentity.PublicKey)
	})
	if err != nil {
		t.Fatalf("ServerTLSConfig() error = %v", err)
	}
	serverTLS.Time = func() time.Time { return labTime }
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	lab := &tunnelLab{
		ctx: ctx, cancel: cancel, listener: listener, serverTLS: serverTLS,
		dialer: &echoServiceDialer{}, states: make(chan tunnel.ServerState, 32), done: make(chan error, 1),
	}
	lab.active.Store(1)
	lab.wait.Add(1)
	go func() {
		defer lab.wait.Done()
		lab.active.Add(1)
		defer lab.active.Add(-1)
		server := &tunnel.Server{Accept: lab.accept, Dialer: lab.dialer, OnState: func(state tunnel.ServerState) { lab.states <- state }}
		lab.done <- server.Run(ctx)
	}()
	return lab
}

func (l *tunnelLab) accept(ctx context.Context) (tunnel.Session, error) {
	for {
		raw, err := l.listener.Accept()
		if err != nil {
			return nil, err
		}
		l.active.Add(1)
		connection := &accountedConn{Conn: raw, active: &l.active}
		secured := tls.Server(connection, l.serverTLS.Clone())
		if err := secured.HandshakeContext(ctx); err != nil {
			_ = secured.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		return tunnel.NewServerSession(secured)
	}
}

func (l *tunnelLab) dial(t *testing.T, identity tunnel.Identity, peer ed25519.PublicKey) tunnel.Session {
	t.Helper()
	session, err := l.tryDial(identity, peer)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	return session
}

func (l *tunnelLab) tryDial(identity tunnel.Identity, peer ed25519.PublicKey) (tunnel.Session, error) {
	clientTLS, err := tunnel.ClientTLSConfig(identity, peer)
	if err != nil {
		return nil, err
	}
	clientTLS.Time = func() time.Time { return labTime }
	raw, err := (&net.Dialer{}).DialContext(l.ctx, "tcp4", l.listener.Addr().String())
	if err != nil {
		return nil, err
	}
	secured := tls.Client(raw, clientTLS)
	if err := secured.HandshakeContext(l.ctx); err != nil {
		_ = secured.Close()
		return nil, err
	}
	return tunnel.NewClientSession(secured)
}

func (l *tunnelLab) waitState(t *testing.T, wanted tunnel.ServerState) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case state := <-l.states:
			if state == wanted {
				return
			}
		case <-deadline:
			t.Fatalf("server did not reach state %s", wanted)
		}
	}
}

func (l *tunnelLab) close(t *testing.T) {
	t.Helper()
	l.closeOnce.Do(func() {
		l.cancel()
		_ = l.listener.Close()
		l.active.Add(-1)
	})
	select {
	case err := <-l.done:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("tunnel server shutdown error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tunnel server did not stop")
	}
	finished := make(chan struct{})
	go func() { l.wait.Wait(); l.dialer.wait.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("owned transport goroutines did not stop")
	}
}

type accountedConn struct {
	net.Conn
	once   sync.Once
	active *atomic.Int32
}

func (c *accountedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.active.Add(-1) })
	return err
}

type echoServiceDialer struct {
	mu     sync.Mutex
	routed []tunnel.StreamKind
	wait   sync.WaitGroup
}

func (d *echoServiceDialer) DialService(ctx context.Context, kind tunnel.StreamKind) (net.Conn, error) {
	client, service := net.Pipe()
	d.mu.Lock()
	d.routed = append(d.routed, kind)
	d.mu.Unlock()
	d.wait.Add(1)
	go func() {
		defer d.wait.Done()
		defer service.Close()
		buffer := make([]byte, 4096)
		for {
			_ = service.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			count, err := service.Read(buffer)
			if count > 0 {
				if _, writeErr := service.Write(buffer[:count]); writeErr != nil {
					return
				}
			}
			if err != nil {
				if networkErr, ok := err.(net.Error); ok && networkErr.Timeout() && ctx.Err() == nil {
					continue
				}
				return
			}
		}
	}()
	return client, nil
}

func (d *echoServiceDialer) kinds() []tunnel.StreamKind {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]tunnel.StreamKind(nil), d.routed...)
}

func assertEcho(t *testing.T, session tunnel.Session, kind tunnel.StreamKind, payload []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := session.OpenStream(ctx, kind)
	if err != nil {
		t.Errorf("OpenStream(%s) error = %v", kind, err)
		return
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := stream.Write(payload); err != nil {
		t.Errorf("Write(%s) error = %v", kind, err)
		return
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(stream, received); err != nil {
		t.Errorf("Read(%s) error = %v", kind, err)
		return
	}
	if !bytes.Equal(received, payload) {
		t.Errorf("echo(%s) = %q, want %q", kind, received, payload)
	}
}

func deterministicIdentity(marker byte) tunnel.Identity {
	seed := bytes.Repeat([]byte{marker}, ed25519.SeedSize)
	privateKey := ed25519.NewKeyFromSeed(seed)
	return tunnel.Identity{PrivateKey: privateKey, PublicKey: append(ed25519.PublicKey(nil), privateKey.Public().(ed25519.PublicKey)...)}
}

func sameKindCounts(got, want []tunnel.StreamKind) bool {
	counts := func(values []tunnel.StreamKind) map[tunnel.StreamKind]int {
		result := make(map[tunnel.StreamKind]int)
		for _, value := range values {
			result[value]++
		}
		return result
	}
	return reflect.DeepEqual(counts(got), counts(want))
}
