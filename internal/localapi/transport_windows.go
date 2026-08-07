//go:build windows

package localapi

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

func listenLocal(endpoint string) (net.Listener, error) {
	return winio.ListenPipe(endpoint, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;GA;;;OW)",
		MessageMode:        false,
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
}

func dialLocalEndpoint(ctx context.Context, endpoint string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, endpoint)
}

// The pipe ACL admits only SYSTEM and the pipe owner, so authorization occurs
// before the connection reaches the server.
func authorizeCurrentUser(net.Conn) error { return nil }
