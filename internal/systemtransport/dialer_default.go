//go:build !darwin

package systemtransport

import (
	"context"
	"errors"
	"net"
	"strconv"
)

// DialContextFunc matches net.Dialer's context-aware dialing method.
type DialContextFunc = func(context.Context, string, string) (net.Conn, error)

// PairingDialContext returns the platform pairing dialer.
func PairingDialContext() DialContextFunc {
	return (&net.Dialer{}).DialContext
}

func TunnelDialContext() DialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, errors.New("tunnel transport requires TCP")
		}
		host, portText, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("tunnel target must contain a literal address and port")
		}
		ip := net.ParseIP(host)
		port, portErr := strconv.Atoi(portText)
		if ip == nil || ip.IsUnspecified() || (!ip.IsPrivate() && !ip.IsLoopback()) || portErr != nil || port != 49221 {
			return nil, errors.New("tunnel target must use a private address on TCP 49221")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
}
