//go:build windows

package localapi

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

func listenLocal(endpoint string) (net.Listener, error) {
	return winio.ListenPipe(endpoint, &winio.PipeConfig{
		SecurityDescriptor: ownerOnlyPipeSecurityDescriptor(),
		MessageMode:        false,
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
}

func dialLocalEndpoint(ctx context.Context, endpoint string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, endpoint)
}

// The owner-only pipe DACL enforces the Windows current-user boundary before a
// connection reaches Server. This hook intentionally performs no second check.
func authorizeCurrentUser(net.Conn) error { return nil }
