package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
)

// ErrLocalPortOccupied distinguishes a local ownership conflict from a
// transient tunnel failure. The client must not retry indefinitely while a
// foreign process owns one of its fixed loopback ports.
var ErrLocalPortOccupied = errors.New("required local tunnel port is occupied")

const (
	SyncRelayPort    = 49220
	DockerRelayPort  = 49222
	ControlRelayPort = 49223
	MetricsRelayPort = 49224
)

func StartLoopbackRelays(ctx context.Context, session Session) ([]net.Listener, error) {
	if ctx == nil || session == nil {
		return nil, fmt.Errorf("loopback relay dependencies are incomplete")
	}
	targets := []struct {
		port int
		kind StreamKind
	}{
		{SyncRelayPort, StreamWorkspaceSync},
		{DockerRelayPort, StreamDockerSSH},
		{ControlRelayPort, StreamControl},
		{MetricsRelayPort, StreamMetrics},
	}
	listeners := make([]net.Listener, 0, len(targets))
	for _, target := range targets {
		listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(target.port)))
		if err != nil {
			closeListeners(listeners)
			return nil, fmt.Errorf("%w: listen on loopback port %d", ErrLocalPortOccupied, target.port)
		}
		listeners = append(listeners, listener)
		go acceptRelay(ctx, listener, session, target.kind)
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-session.Done():
		}
		closeListeners(listeners)
	}()
	return listeners, nil
}

func acceptRelay(ctx context.Context, listener net.Listener, session Session, kind StreamKind) {
	for {
		local, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			stream, openErr := session.OpenStream(ctx, kind)
			if openErr != nil {
				_ = local.Close()
				return
			}
			joinConnections(ctx, local, stream)
		}()
	}
}

func closeListeners(listeners []net.Listener) {
	for _, listener := range listeners {
		_ = listener.Close()
	}
}
