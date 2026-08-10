package sshtransport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// ForwardDirection distinguishes remote-service access from explicit Mac-port sharing.
type ForwardDirection string

const (
	ForwardLocal   ForwardDirection = "local"
	ForwardReverse ForwardDirection = "reverse"
)

// ForwardSpec always binds both ends to loopback.
type ForwardSpec struct {
	Direction  ForwardDirection
	LocalPort  int
	RemotePort int
}

// PortConflictError reports an occupied Mac port without touching its owner.
type PortConflictError struct{ Port int }

func (e *PortConflictError) Error() string {
	return fmt.Sprintf("TCP port %d is already in use on Mac", e.Port)
}

// Forwarder starts strict app-owned SSH forwarding processes.
type Forwarder struct {
	Runner CommandRunner
	Binary string
	Env    []string
	Stderr io.Writer
	Probe  func(context.Context, int) error
}

// ManagedForward owns one SSH child and broadcasts its completion.
type ManagedForward struct {
	process   Process
	done      chan struct{}
	err       error
	closeOnce sync.Once
}

// Start probes local forwards and invokes ssh without a shell.
func (f Forwarder) Start(ctx context.Context, configPath, managedHost string, spec ForwardSpec) (*ManagedForward, error) {
	if !filepath.IsAbs(configPath) {
		return nil, errors.New("managed SSH config path must be absolute")
	}
	if strings.TrimSpace(managedHost) == "" || strings.HasPrefix(managedHost, "-") || strings.ContainsAny(managedHost, " \t\r\n") {
		return nil, errors.New("invalid managed SSH host alias")
	}
	if spec.LocalPort < 1 || spec.LocalPort > 65535 || spec.RemotePort < 1 || spec.RemotePort > 65535 {
		return nil, errors.New("invalid SSH forward port")
	}
	flag := ""
	value := ""
	switch spec.Direction {
	case ForwardLocal:
		probe := f.Probe
		if probe == nil {
			probe = probeLoopbackPort
		}
		if err := probe(ctx, spec.LocalPort); err != nil {
			var conflict *PortConflictError
			if errors.As(err, &conflict) {
				return nil, conflict
			}
			return nil, fmt.Errorf("probe local forward port %d: %w", spec.LocalPort, err)
		}
		flag = "-L"
		value = "127.0.0.1:" + strconv.Itoa(spec.LocalPort) + ":127.0.0.1:" + strconv.Itoa(spec.RemotePort)
	case ForwardReverse:
		flag = "-R"
		value = "127.0.0.1:" + strconv.Itoa(spec.RemotePort) + ":127.0.0.1:" + strconv.Itoa(spec.LocalPort)
	default:
		return nil, errors.New("invalid SSH forward direction")
	}

	runner := f.Runner
	if runner == nil {
		runner = execCommandRunner{}
	}
	binary := f.Binary
	if binary == "" {
		binary = "ssh"
	}
	process, err := runner.Start(ctx, Command{
		Binary: binary,
		Args: []string{
			"-F", configPath,
			"-N",
			"-o", "ExitOnForwardFailure=yes",
			flag, value,
			managedHost,
		},
		Env: f.Env, Stderr: f.Stderr,
	})
	if err != nil {
		return nil, fmt.Errorf("start SSH forward: %w", err)
	}
	managed := &ManagedForward{process: process, done: make(chan struct{})}
	go func() {
		managed.err = process.Wait()
		close(managed.done)
	}()
	return managed, nil
}

// Done closes when the owned SSH process exits.
func (f *ManagedForward) Done() <-chan struct{} { return f.done }

// Err returns the process result after Done closes.
func (f *ManagedForward) Err() error { return f.err }

// Close terminates only the owned SSH child and waits for it.
func (f *ManagedForward) Close() error {
	if f == nil || f.process == nil {
		return nil
	}
	var killErr error
	f.closeOnce.Do(func() { killErr = f.process.Kill() })
	<-f.done
	if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		return killErr
	}
	return nil
}

func probeLoopbackPort(ctx context.Context, port int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return &PortConflictError{Port: port}
	}
	return listener.Close()
}
