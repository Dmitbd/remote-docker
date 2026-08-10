package transportlab_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/tunnel"
)

type faultMode int

const (
	faultPass faultMode = iota
	faultDelayed
	faultResetFirstWrite
	faultResetMidstream
)

type faultConn struct {
	net.Conn
	mode    faultMode
	written int
	mu      sync.Mutex
}

func (c *faultConn) Write(payload []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.mode {
	case faultDelayed:
		time.Sleep(10 * time.Millisecond)
	case faultResetFirstWrite:
		_ = c.Conn.Close()
		return 0, net.ErrClosed
	case faultResetMidstream:
		if c.written > 0 {
			_ = c.Conn.Close()
			return 0, net.ErrClosed
		}
	}
	count, err := c.Conn.Write(payload)
	c.written += count
	return count, err
}

func TestTransportFilterModesUseRealTLSYamuxClientReconnectAndCleanup(t *testing.T) {
	for _, mode := range []faultMode{faultPass, faultDelayed, faultResetFirstWrite, faultResetMidstream} {
		t.Run(fmt.Sprintf("mode-%d", mode), func(t *testing.T) {
			macIdentity := deterministicIdentity(11)
			windowsIdentity := deterministicIdentity(12)
			lab := newTunnelLab(t, macIdentity, windowsIdentity)
			clientTLS, err := tunnel.ClientTLSConfig(macIdentity, windowsIdentity.PublicKey)
			if err != nil {
				t.Fatal(err)
			}
			clientTLS.Time = func() time.Time { return labTime }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := make(chan struct{}, 4)
			var reconnectStates atomic.Int32
			var disconnectedStates atomic.Int32
			var attempts atomic.Int32
			var currentMu sync.Mutex
			var current net.Conn
			client := &tunnel.Client{
				Dial: func(dialCtx context.Context) (net.Conn, error) {
					raw, err := (&net.Dialer{}).DialContext(dialCtx, "tcp4", lab.listener.Addr().String())
					if err != nil {
						return nil, err
					}
					lab.active.Add(1)
					accounted := &accountedConn{Conn: raw, active: &lab.active}
					selected := faultPass
					if attempts.Add(1) == 1 {
						selected = mode
					}
					filtered := &faultConn{Conn: accounted, mode: selected}
					secured := tls.Client(filtered, clientTLS.Clone())
					if err := secured.HandshakeContext(dialCtx); err != nil {
						_ = secured.Close()
						return nil, err
					}
					currentMu.Lock()
					current = secured
					currentMu.Unlock()
					return secured, nil
				},
				OpenRelays: func(session tunnel.Session) ([]io.Closer, error) {
					for _, kind := range []tunnel.StreamKind{
						tunnel.StreamDockerSSH, tunnel.StreamWorkspaceSync, tunnel.StreamControl, tunnel.StreamMetrics,
					} {
						if err := echoOverSession(ctx, session, kind, []byte("fault-lab-"+kind.String())); err != nil {
							return nil, err
						}
					}
					ready <- struct{}{}
					return nil, nil
				},
				OnState: func(state tunnel.ClientState, _ error) {
					if state == tunnel.ClientReconnecting {
						reconnectStates.Add(1)
					}
					if state == tunnel.ClientDisconnected {
						disconnectedStates.Add(1)
					}
				},
				Wait: func(waitCtx context.Context, _ time.Duration) bool { return waitCtx.Err() == nil },
			}
			clientDone := make(chan error, 1)
			lab.active.Add(1)
			go func() {
				defer lab.active.Add(-1)
				clientDone <- client.Run(ctx)
			}()
			waitReady(t, ready, "initial recovered channels")
			currentMu.Lock()
			activeConnection := current
			currentMu.Unlock()
			if activeConnection == nil {
				t.Fatal("client had no authenticated connection to interrupt")
			}
			_ = activeConnection.Close()
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out waiting for channels after forced disconnect: attempts=%d disconnected=%d reconnecting=%d", attempts.Load(), disconnectedStates.Load(), reconnectStates.Load())
			}
			cancel()
			select {
			case err := <-clientDone:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("client shutdown error = %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("real tunnel client did not stop")
			}
			lab.close(t)
			if active := lab.active.Load(); active != 0 {
				t.Fatalf("fault lab retained %d owned resources", active)
			}
			if mode == faultResetFirstWrite || mode == faultResetMidstream {
				if attempts.Load() < 3 {
					t.Fatalf("fault mode %d used %d connections, want failed attempt plus two sessions", mode, attempts.Load())
				}
			}
			if reconnectStates.Load() == 0 {
				t.Fatal("real tunnel.Client reconnect loop was not observed")
			}
		})
	}
}

func echoOverSession(ctx context.Context, session tunnel.Session, kind tunnel.StreamKind, payload []byte) error {
	streamCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	stream, err := session.OpenStream(streamCtx, kind)
	if err != nil {
		return err
	}
	defer stream.Close()
	if err := stream.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	if _, err := stream.Write(payload); err != nil {
		return err
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(stream, received); err != nil {
		return err
	}
	if !bytes.Equal(received, payload) {
		return errors.New("tunnel echo payload changed")
	}
	return nil
}

func waitReady(t *testing.T, ready <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}
