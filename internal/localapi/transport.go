package localapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
)

func DefaultEndpoint() (string, error) {
	if runtime.GOOS == "windows" {
		current, err := user.Current()
		if err != nil {
			return "", fmt.Errorf("identify current user: %w", err)
		}
		digest := sha256.Sum256([]byte(current.Username))
		return fmt.Sprintf(`\\.\pipe\remote-docker-agent-%x`, digest[:8]), nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find user cache directory: %w", err)
	}
	return filepath.Join(cache, "RemoteDocker", "agent.sock"), nil
}

func Listen(endpoint string) (net.Listener, error) {
	if endpoint == "" {
		var err error
		endpoint, err = DefaultEndpoint()
		if err != nil {
			return nil, err
		}
	}
	return listenLocal(endpoint)
}

func dialLocal(ctx context.Context, endpoint string) (net.Conn, error) {
	if endpoint == "" {
		var err error
		endpoint, err = DefaultEndpoint()
		if err != nil {
			return nil, err
		}
	}
	return dialLocalEndpoint(ctx, endpoint)
}
