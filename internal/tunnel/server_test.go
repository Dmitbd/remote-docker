package tunnel

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestTunnelServerRejectsSecondSessionAndNeverDialsUnknownKind(t *testing.T) {
	first := newFakeSession()
	second := newFakeSession()
	accepts := make(chan Session, 2)
	accepts <- first
	accepts <- second
	dialer := &recordingServiceDialer{}
	var statesMu sync.Mutex
	var states []ServerState
	server := &Server{
		Accept: func(ctx context.Context) (Session, error) {
			select {
			case session := <-accepts:
				return session, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		Dialer:  dialer,
		OnState: func(state ServerState) { statesMu.Lock(); states = append(states, state); statesMu.Unlock() },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	select {
	case <-second.Done():
	case <-time.After(time.Second):
		t.Fatal("second authenticated session was not rejected")
	}
	first.streams <- fakeAcceptedStream{kind: 99, connection: pipeClosedPeer(t)}
	time.Sleep(10 * time.Millisecond)
	if dialer.calls() != 0 {
		t.Fatalf("unknown stream dialed WSL %d times", dialer.calls())
	}
	cancel()
	_ = first.Close()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not clean up")
	}
}

type fakeAcceptedStream struct {
	kind       StreamKind
	connection net.Conn
}

type fakeSession struct {
	streams chan fakeAcceptedStream
	done    chan struct{}
	once    sync.Once
}

func newFakeSession() *fakeSession {
	return &fakeSession{streams: make(chan fakeAcceptedStream, 4), done: make(chan struct{})}
}

func (s *fakeSession) OpenStream(context.Context, StreamKind) (net.Conn, error) {
	return nil, errors.New("not a client session")
}
func (s *fakeSession) AcceptStream(ctx context.Context) (StreamKind, net.Conn, error) {
	select {
	case stream := <-s.streams:
		return stream.kind, stream.connection, nil
	case <-s.done:
		return 0, nil, net.ErrClosed
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}
func (s *fakeSession) Done() <-chan struct{} { return s.done }
func (s *fakeSession) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

type recordingServiceDialer struct {
	mu    sync.Mutex
	kinds []StreamKind
}

func (d *recordingServiceDialer) DialService(context.Context, StreamKind) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.kinds = append(d.kinds, StreamDockerSSH)
	return nil, errors.New("not available")
}
func (d *recordingServiceDialer) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.kinds)
}

func pipeClosedPeer(t *testing.T) net.Conn {
	t.Helper()
	first, second := net.Pipe()
	_ = second.Close()
	return first
}
