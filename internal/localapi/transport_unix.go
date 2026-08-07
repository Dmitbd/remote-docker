//go:build !windows

package localapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

func listenLocal(endpoint string) (net.Listener, error) {
	if !filepath.IsAbs(endpoint) {
		return nil, errors.New("local control socket path must be absolute")
	}
	directory := filepath.Dir(endpoint)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create local control directory: %w", err)
	}
	if err := requireCurrentUserPath(directory, true); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(endpoint); err == nil {
		if err := requireCurrentUserPath(endpoint, false); err != nil {
			return nil, err
		}
		if err := os.Remove(endpoint); err != nil {
			return nil, fmt.Errorf("remove stale local control socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect local control socket: %w", err)
	}
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		return nil, fmt.Errorf("listen on local control socket: %w", err)
	}
	if err := os.Chmod(endpoint, 0o600); err != nil {
		listener.Close()
		_ = os.Remove(endpoint)
		return nil, fmt.Errorf("protect local control socket: %w", err)
	}
	return &removingListener{Listener: listener, path: endpoint}, nil
}

func dialLocalEndpoint(ctx context.Context, endpoint string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
}

func requireCurrentUserPath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect local control path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("local control path must not be a symlink")
	}
	if directory {
		if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return errors.New("local control directory must be private")
		}
	} else if info.Mode()&os.ModeSocket == 0 {
		return errors.New("local control endpoint exists and is not a socket")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return ErrPeerOwnership
	}
	return nil
}

type removingListener struct {
	net.Listener
	path string
}

func (l *removingListener) Close() error {
	err := l.Listener.Close()
	removeErr := os.Remove(l.path)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
		return removeErr
	}
	return err
}
