//go:build !darwin

package systemtransport

import (
	"context"
	"net"
)

// DialContextFunc matches net.Dialer's context-aware dialing method.
type DialContextFunc = func(context.Context, string, string) (net.Conn, error)

// PairingDialContext returns the platform pairing dialer.
func PairingDialContext() DialContextFunc {
	return (&net.Dialer{}).DialContext
}
