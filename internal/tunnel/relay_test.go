package tunnel

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestLoopbackRelaysBindOnlyFixedIPv4PortsAndStopCleanly(t *testing.T) {
	session := newRecordingOpenSession()
	ctx, cancel := context.WithCancel(context.Background())
	listeners, err := StartLoopbackRelays(ctx, session)
	if err != nil {
		t.Fatalf("StartLoopbackRelays() error = %v", err)
	}
	if len(listeners) != 4 {
		t.Fatalf("listener count = %d", len(listeners))
	}
	wants := map[int]StreamKind{SyncRelayPort: StreamWorkspaceSync, DockerRelayPort: StreamDockerSSH, ControlRelayPort: StreamControl, MetricsRelayPort: StreamMetrics}
	for _, listener := range listeners {
		address := listener.Addr().(*net.TCPAddr)
		if !address.IP.Equal(net.ParseIP("127.0.0.1")) {
			t.Fatalf("relay bound non-loopback address %s", address)
		}
		connection, dialErr := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(address.Port)))
		if dialErr != nil {
			t.Fatalf("Dial(%s) error = %v", address, dialErr)
		}
		_ = connection.Close()
		if session.waitKind(t) != wants[address.Port] {
			t.Fatalf("relay %d opened wrong stream", address.Port)
		}
	}
	cancel()
	_ = session.Close()
	deadline := time.Now().Add(time.Second)
	for _, port := range []int{SyncRelayPort, DockerRelayPort, ControlRelayPort, MetricsRelayPort} {
		for time.Now().Before(deadline) {
			connection, dialErr := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 10*time.Millisecond)
			if dialErr != nil {
				break
			}
			_ = connection.Close()
		}
		if connection, dialErr := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 10*time.Millisecond); dialErr == nil {
			_ = connection.Close()
			t.Fatalf("relay port %d remained open", port)
		}
	}
}

func TestLoopbackRelaysFailWithoutClosingUnrelatedPortOwner(t *testing.T) {
	owner, err := net.Listen("tcp4", "127.0.0.1:49222")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer owner.Close()
	if _, err := StartLoopbackRelays(context.Background(), newRecordingOpenSession()); err == nil {
		t.Fatal("StartLoopbackRelays accepted occupied port")
	}
	connection, err := net.Dial("tcp4", owner.Addr().String())
	if err != nil {
		t.Fatalf("unrelated owner was closed: %v", err)
	}
	_ = connection.Close()
}

type recordingOpenSession struct {
	kinds chan StreamKind
	done  chan struct{}
}

func newRecordingOpenSession() *recordingOpenSession {
	return &recordingOpenSession{kinds: make(chan StreamKind, 8), done: make(chan struct{})}
}
func (s *recordingOpenSession) OpenStream(_ context.Context, kind StreamKind) (net.Conn, error) {
	s.kinds <- kind
	first, second := net.Pipe()
	_ = second.Close()
	return first, nil
}
func (s *recordingOpenSession) AcceptStream(context.Context) (StreamKind, net.Conn, error) { return 0, nil, errors.New("unsupported") }
func (s *recordingOpenSession) Done() <-chan struct{} { return s.done }
func (s *recordingOpenSession) Close() error {
	select { case <-s.done: default: close(s.done) }
	return nil
}
func (s *recordingOpenSession) waitKind(t *testing.T) StreamKind {
	t.Helper()
	select {
	case kind := <-s.kinds: return kind
	case <-time.After(time.Second): t.Fatal("relay did not open stream"); return 0
	}
}
