//go:build linux

package localapi

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func authorizeCurrentUser(connection net.Conn) error {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return ErrPeerOwnership
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return ErrPeerOwnership
	}
	var credential *unix.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credential, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || socketErr != nil || credential == nil {
		return ErrPeerOwnership
	}
	if int(credential.Uid) != os.Geteuid() {
		return ErrPeerOwnership
	}
	return nil
}
