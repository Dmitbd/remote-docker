package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/Dmitbd/remote-docker/internal/dockercli"
	"github.com/Dmitbd/remote-docker/internal/workspace"
)

const defaultPreflightTimeout = 120 * time.Second

// AnalysisProvider parses one real Docker CLI invocation.
type AnalysisProvider interface {
	Analyze(context.Context, dockercli.Invocation, dockercli.Executor) (dockercli.Analysis, error)
}

// BindResolver applies the registered workspace policy to one source.
type BindResolver interface {
	Resolve(source, cwd string) (workspace.ResolvedPath, error)
}

// SyncReadiness prepares and waits for one exact-path workspace folder on both peers.
type SyncReadiness interface {
	EnsureFolder(context.Context, workspace.ResolvedPath) error
	Scan(context.Context, workspace.ResolvedPath) error
	WaitBoth(context.Context, workspace.ResolvedPath) error
}

// PortProbe checks whether a requested fixed Mac host port is still available.
type PortProbe interface {
	Probe(context.Context, dockercli.Port) error
}

// Preflight gates the real Docker process on workspace and port readiness.
type Preflight struct {
	Analyzer AnalysisProvider
	Resolver BindResolver
	Sync     SyncReadiness
	Ports    PortProbe
	Timeout  time.Duration
}

// RegisteredWorkspaceResolver is the production adapter for workspace.ResolveBind.
type RegisteredWorkspaceResolver struct {
	Workspaces []workspace.Workspace
}

func (r RegisteredWorkspaceResolver) Resolve(source, cwd string) (workspace.ResolvedPath, error) {
	return workspace.ResolveBind(source, cwd, r.Workspaces)
}

// Check performs all non-mutating gates and sync preparation before Docker starts.
func (p *Preflight) Check(ctx context.Context, invocation dockercli.Invocation, real dockercli.Executor, progress io.Writer) error {
	if p == nil {
		return nil
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultPreflightTimeout
	}
	preflightCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var analysis dockercli.Analysis
	var err error
	if p.Analyzer == nil {
		analysis, err = dockercli.Analyze(preflightCtx, invocation, real)
	} else {
		analysis, err = p.Analyzer.Analyze(preflightCtx, invocation, real)
	}
	if err != nil {
		return fmt.Errorf("analyze Docker command: %w", err)
	}
	if len(analysis.Unsupported) > 0 {
		reason := analysis.Unsupported[0]
		return fmt.Errorf("Docker feature %s is not supported: %s", reason.Code, reason.Detail)
	}

	resolved := make([]workspace.ResolvedPath, 0, len(analysis.BindSources))
	seenRemote := make(map[string]struct{}, len(analysis.BindSources))
	for _, source := range analysis.BindSources {
		if p.Resolver == nil {
			return fmt.Errorf("bind source %s is not registered; run: remote-docker workspace add %s", source, source)
		}
		path, resolveErr := p.Resolver.Resolve(source, invocation.Dir)
		if resolveErr != nil {
			return fmt.Errorf("bind source %s is not registered: %w; run: remote-docker workspace add %s", source, resolveErr, source)
		}
		if _, exists := seenRemote[path.Remote]; exists {
			continue
		}
		seenRemote[path.Remote] = struct{}{}
		resolved = append(resolved, path)
	}

	if len(resolved) > 0 && p.Sync == nil {
		return errors.New("workspace sync is unavailable; run: remote-docker sync status")
	}
	for _, path := range resolved {
		if err := p.Sync.EnsureFolder(preflightCtx, path); err != nil {
			return fmt.Errorf("prepare sync folder %s: %w; run: remote-docker sync status", path.Local, err)
		}
	}
	for _, path := range resolved {
		if err := p.Sync.Scan(preflightCtx, path); err != nil {
			return fmt.Errorf("scan workspace %s: %w; run: remote-docker sync status", path.Local, err)
		}
	}
	for _, path := range resolved {
		if err := waitForBoth(preflightCtx, p.Sync, path, progress); err != nil {
			return fmt.Errorf("wait for workspace %s: %w; run: remote-docker sync status", path.Local, err)
		}
	}

	portProbe := p.Ports
	if portProbe == nil {
		portProbe = TCPPortProbe{}
	}
	for _, port := range analysis.StaticTCPPorts {
		if err := portProbe.Probe(preflightCtx, port); err != nil {
			return err
		}
	}
	return nil
}

func waitForBoth(ctx context.Context, sync SyncReadiness, path workspace.ResolvedPath, progress io.Writer) error {
	if progress == nil {
		return sync.WaitBoth(ctx, path)
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Fprintf(progress, "remote-docker: waiting for sync %s\n", path.Local)
			}
		}
	}()
	err := sync.WaitBoth(ctx, path)
	close(done)
	<-finished
	return err
}

// TCPPortProbe reserves no port; it opens and immediately closes a loopback listener.
type TCPPortProbe struct{}

func (TCPPortProbe) Probe(ctx context.Context, port dockercli.Port) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port.HostPort))
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("TCP port %d is already in use on Mac; choose another host port or stop the owning application", port.HostPort)
	}
	if err := listener.Close(); err != nil {
		return fmt.Errorf("release TCP port probe %d: %w", port.HostPort, err)
	}
	return nil
}
