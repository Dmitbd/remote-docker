//go:build !darwin && !linux && !windows

package localapi

import "net"

func authorizeCurrentUser(net.Conn) error {
	return ErrPeerOwnership
}
