package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/dockercli"
	"github.com/Dmitbd/remote-docker/internal/workspace"
)

func TestPreflightRunsExactOrderBeforeDocker(t *testing.T) {
	events := []string{}
	analyzer := analysisFunc(func(context.Context, dockercli.Invocation, dockercli.Executor) (dockercli.Analysis, error) {
		events = append(events, "analyze")
		return dockercli.Analysis{
			NeedsEngine:    true,
			BindSources:    []string{"./src"},
			StaticTCPPorts: []dockercli.Port{{HostPort: 8080, ContainerPort: 80}},
		}, nil
	})
	resolver := &orderedResolver{events: &events, resolved: workspace.ResolvedPath{
		Local: "/Users/demo/project/src", Remote: "/Users/demo/project/src",
		WorkspaceID: "project", Mode: workspace.PathModeWorkspace,
	}}
	sync := &orderedSync{events: &events}
	ports := &orderedPortProbe{events: &events}
	executor := &orderedDockerExecutor{events: &events}
	preflight := &Preflight{Analyzer: analyzer, Resolver: resolver, Sync: sync, Ports: ports}

	code := RunRuntime(context.Background(), Runtime{
		ProgramName:   "docker",
		DockerCLIPath: "/bundle/docker-real",
		Executor:      executor,
		Preflight:     preflight,
		Dir:           "/Users/demo/project",
	}, []string{"run", "-v", "./src:/app", "-p", "8080:80", "alpine"}, io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("RunRuntime() code = %d", code)
	}
	want := []string{"analyze", "resolve", "ensure", "scan", "wait", "port", "execute"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestPreflightSkipsSyncForCommandsWithoutBinds(t *testing.T) {
	events := []string{}
	executor := &orderedDockerExecutor{events: &events}
	preflight := &Preflight{Analyzer: analysisFunc(func(context.Context, dockercli.Invocation, dockercli.Executor) (dockercli.Analysis, error) {
		events = append(events, "analyze")
		return dockercli.Analysis{NeedsEngine: true}, nil
	})}
	code := RunRuntime(context.Background(), Runtime{
		ProgramName: "docker", DockerCLIPath: "/bundle/docker-real", Executor: executor, Preflight: preflight,
	}, []string{"ps"}, io.Discard, io.Discard)
	if code != 0 || !reflect.DeepEqual(events, []string{"analyze", "execute"}) {
		t.Fatalf("code=%d events=%#v", code, events)
	}
}

func TestPreflightFailuresNeverExecuteDocker(t *testing.T) {
	baseAnalysis := dockercli.Analysis{BindSources: []string{"/outside"}, StaticTCPPorts: []dockercli.Port{{HostPort: 8080}}}
	tests := []struct {
		name       string
		analysis   dockercli.Analysis
		resolveErr error
		ensureErr  error
		waitErr    error
		portErr    error
		want       string
	}{
		{
			name: "unsupported",
			analysis: dockercli.Analysis{Unsupported: []dockercli.Reason{{
				Code: dockercli.ReasonUnsupportedUDP, Detail: "UDP is unsupported",
			}}},
			want: "UDP is unsupported",
		},
		{name: "unknown path", analysis: baseAnalysis, resolveErr: workspace.ErrOutsideWorkspace, want: "remote-docker workspace add /outside"},
		{name: "sync conflict", analysis: baseAnalysis, ensureErr: errors.New("folder conflict"), want: "folder conflict"},
		{name: "sync timeout", analysis: baseAnalysis, waitErr: context.DeadlineExceeded, want: "remote-docker sync status"},
		{name: "port conflict", analysis: baseAnalysis, portErr: errors.New("TCP port 8080 is already in use"), want: "TCP port 8080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := &capturingExecutor{}
			resolver := &orderedResolver{resolved: workspace.ResolvedPath{
				Local: "/outside", Remote: "/outside", WorkspaceID: "workspace", Mode: workspace.PathModeWorkspace,
			}, err: tt.resolveErr}
			preflight := &Preflight{
				Analyzer: analysisFunc(func(context.Context, dockercli.Invocation, dockercli.Executor) (dockercli.Analysis, error) {
					return tt.analysis, nil
				}),
				Resolver: resolver,
				Sync:     &orderedSync{ensureErr: tt.ensureErr, waitErr: tt.waitErr},
				Ports:    &orderedPortProbe{err: tt.portErr},
			}
			var stderr bytes.Buffer
			code := RunRuntime(context.Background(), Runtime{
				ProgramName: "docker", DockerCLIPath: "/bundle/docker-real", Executor: executor,
				Preflight: preflight, Dir: "/workspace", Env: []string{"SECRET=must-not-leak"},
			}, []string{"run", "alpine"}, io.Discard, &stderr)
			if code == 0 || executor.called {
				t.Fatalf("code=%d Docker called=%v", code, executor.called)
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.want)
			}
			if strings.Contains(stderr.String(), "must-not-leak") {
				t.Fatalf("stderr leaked environment: %q", stderr.String())
			}
		})
	}
}

func TestPreflightCtrlCCancelsBeforeDockerExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	executor := &capturingExecutor{}
	preflight := &Preflight{
		Analyzer: analysisFunc(func(context.Context, dockercli.Invocation, dockercli.Executor) (dockercli.Analysis, error) {
			return dockercli.Analysis{BindSources: []string{"./src"}}, nil
		}),
		Resolver: &orderedResolver{resolved: workspace.ResolvedPath{
			Local: "/workspace/src", Remote: "/workspace/src", WorkspaceID: "workspace", Mode: workspace.PathModeWorkspace,
		}},
		Sync: cancelAwareSync{},
	}
	code := RunRuntime(ctx, Runtime{
		ProgramName: "docker", DockerCLIPath: "/bundle/docker-real", Executor: executor,
		Preflight: preflight, Dir: "/workspace",
	}, []string{"run", "alpine"}, io.Discard, io.Discard)
	if code != 130 || executor.called {
		t.Fatalf("code=%d Docker called=%v", code, executor.called)
	}
}

type analysisFunc func(context.Context, dockercli.Invocation, dockercli.Executor) (dockercli.Analysis, error)

func (f analysisFunc) Analyze(ctx context.Context, invocation dockercli.Invocation, real dockercli.Executor) (dockercli.Analysis, error) {
	return f(ctx, invocation, real)
}

type orderedResolver struct {
	events   *[]string
	resolved workspace.ResolvedPath
	err      error
}

func (r *orderedResolver) Resolve(_ string, _ string) (workspace.ResolvedPath, error) {
	if r.events != nil {
		*r.events = append(*r.events, "resolve")
	}
	return r.resolved, r.err
}

type orderedSync struct {
	events    *[]string
	ensureErr error
	waitErr   error
}

type cancelAwareSync struct{}

func (cancelAwareSync) EnsureFolder(ctx context.Context, _ workspace.ResolvedPath) error {
	return ctx.Err()
}
func (cancelAwareSync) Scan(ctx context.Context, _ workspace.ResolvedPath) error { return ctx.Err() }
func (cancelAwareSync) WaitBoth(ctx context.Context, _ workspace.ResolvedPath) error {
	<-ctx.Done()
	return ctx.Err()
}

func (s *orderedSync) EnsureFolder(context.Context, workspace.ResolvedPath) error {
	if s.events != nil {
		*s.events = append(*s.events, "ensure")
	}
	return s.ensureErr
}

func (s *orderedSync) Scan(context.Context, workspace.ResolvedPath) error {
	if s.events != nil {
		*s.events = append(*s.events, "scan")
	}
	return nil
}

func (s *orderedSync) WaitBoth(context.Context, workspace.ResolvedPath) error {
	if s.events != nil {
		*s.events = append(*s.events, "wait")
	}
	return s.waitErr
}

type orderedPortProbe struct {
	events *[]string
	err    error
}

func (p *orderedPortProbe) Probe(context.Context, dockercli.Port) error {
	if p.events != nil {
		*p.events = append(*p.events, "port")
	}
	return p.err
}

type orderedDockerExecutor struct{ events *[]string }

func (e *orderedDockerExecutor) Run(context.Context, dockercli.Invocation) error {
	*e.events = append(*e.events, "execute")
	return nil
}
