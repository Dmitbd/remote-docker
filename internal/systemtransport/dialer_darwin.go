//go:build darwin

package systemtransport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	netcatBinary      = "/usr/bin/nc"
	netcatDialTimeout = 5
	netcatCloseWait   = time.Second
	netcatErrorLimit  = 64 << 10
)

// DialContextFunc matches net.Dialer's context-aware dialing method.
type DialContextFunc = func(context.Context, string, string) (net.Conn, error)

type netcatCommandFactory func(string, ...string) *exec.Cmd

type netcatDialer struct {
	command netcatCommandFactory
}

// PairingDialContext returns a dialer backed by Apple's system-signed netcat.
func PairingDialContext() DialContextFunc {
	return (netcatDialer{command: exec.Command}).DialContext
}

// TunnelDialContext permits only the single private-LAN tunnel port.
func TunnelDialContext() DialContextFunc {
	base := (netcatDialer{command: exec.Command}).DialContext
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		_, port, err := validatePairingTarget(network, address)
		if err != nil {
			return nil, err
		}
		if port != 49221 {
			return nil, errors.New("tunnel target must use TCP 49221")
		}
		return base(ctx, network, address)
	}
}

func (d netcatDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ip, port, err := validatePairingTarget(network, address)
	if err != nil {
		return nil, err
	}
	commandFactory := d.command
	if commandFactory == nil {
		commandFactory = exec.Command
	}
	family := "-4"
	if ip.To4() == nil {
		family = "-6"
	}
	command := commandFactory(netcatBinary, family, "-w", strconv.Itoa(netcatDialTimeout), ip.String(), strconv.Itoa(port))
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("prepare system TCP input: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("prepare system TCP output: %w", err)
	}
	stderr := &limitedProcessBuffer{remaining: netcatErrorLimit}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start system TCP client: %w", err)
	}

	client, bridge := net.Pipe()
	managed := &managedProcessConn{
		Conn: client, bridge: bridge, stdin: stdin, stdout: stdout,
		process: command.Process, done: make(chan struct{}),
	}
	go func() {
		_, _ = io.Copy(stdin, bridge)
		_ = stdin.Close()
	}()
	go func() {
		_, _ = io.Copy(bridge, stdout)
		_ = bridge.Close()
	}()
	go func() {
		_ = command.Wait()
		_ = bridge.Close()
		close(managed.done)
	}()

	if err := ctx.Err(); err != nil {
		_ = managed.Close()
		return nil, err
	}
	return managed, nil
}

func validatePairingTarget(network, address string) (net.IP, int, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, 0, errors.New("pairing transport requires TCP")
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, 0, errors.New("pairing target must contain a literal address and port")
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || (!ip.IsPrivate() && !ip.IsLoopback()) {
		return nil, 0, errors.New("pairing target must use a private or loopback address")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, 0, errors.New("pairing target port is invalid")
	}
	if (network == "tcp4" && ip.To4() == nil) || (network == "tcp6" && ip.To4() != nil) {
		return nil, 0, errors.New("pairing target address family does not match network")
	}
	return ip, port, nil
}

type managedProcessConn struct {
	net.Conn
	bridge  net.Conn
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	process *os.Process
	done    chan struct{}
	once    sync.Once
}

func (c *managedProcessConn) Close() error {
	c.once.Do(func() {
		_ = c.Conn.Close()
		_ = c.bridge.Close()
		_ = c.stdin.Close()
		_ = c.stdout.Close()
		if c.process != nil {
			_ = c.process.Kill()
		}
		select {
		case <-c.done:
		case <-time.After(netcatCloseWait):
		}
	})
	return nil
}

type limitedProcessBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	remaining int
}

func (b *limitedProcessBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(value)
	if len(value) > b.remaining {
		value = value[:b.remaining]
	}
	b.remaining -= len(value)
	_, _ = b.buffer.Write(value)
	return original, nil
}

func (b *limitedProcessBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.buffer.String())
}
