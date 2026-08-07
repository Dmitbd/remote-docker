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
	"github.com/Dmitbd/remote-docker/internal/localapi"
)

func TestLocalAgentDockerPreflightBlocksDockerUntilAgentPreparesInvocation(t *testing.T) {
	client := &recordingDockerControlClient{}
	executor := &capturingExecutor{}
	runtime := Runtime{
		ProgramName: "docker", DockerCLIPath: "/bundle/docker-real", ContextName: "remote-docker",
		Executor: executor, Dir: "/Users/demo/project", Env: []string{"SAFE=value"},
		Preflight: LocalAgentDockerPreflight{Client: client},
	}

	code := RunRuntime(context.Background(), runtime, []string{"run", "-v", "/Users/demo/project:/app", "image"}, io.Discard, io.Discard)

	if code != 0 || !executor.called {
		t.Fatalf("RunRuntime() code=%d docker_called=%t, want prepared Docker execution", code, executor.called)
	}
	if client.method != localapi.MethodPrepareDocker {
		t.Fatalf("local method = %q, want %q", client.method, localapi.MethodPrepareDocker)
	}
	params, ok := client.params.(localapi.PrepareDockerParams)
	if !ok || params.WorkingDirectory != "/Users/demo/project" ||
		!reflect.DeepEqual(params.BindSources, []string{"/Users/demo/project"}) || len(params.StaticTCPPorts) != 0 {
		t.Fatalf("prepare params = %#v", client.params)
	}
}

type recordingDockerControlClient struct {
	method localapi.Method
	params any
}

func (c *recordingDockerControlClient) Call(_ context.Context, method localapi.Method, params any, result any) error {
	c.method = method
	c.params = params
	if prepared, ok := result.(*localapi.PrepareDockerResult); ok {
		prepared.Ready = true
	}
	return nil
}

func TestRunRuntimeDelegatesRemoteDockerSubcommand(t *testing.T) {
	executor := &capturingExecutor{err: codedProcessError{code: 23}}
	stdin := strings.NewReader("terminal input")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	runtime := Runtime{
		ProgramName:   "remote-docker",
		DockerCLIPath: "/bundle/docker-real",
		ContextName:   "remote-docker",
		Executor:      executor,
		Env:           []string{"TERM=xterm-256color"},
		Dir:           "/workspace",
		Stdin:         stdin,
	}

	code := RunRuntime(
		context.Background(),
		runtime,
		[]string{"docker", "compose", "ps"},
		&stdout,
		&stderr,
	)

	if code != 23 {
		t.Fatalf("RunRuntime() code = %d, want 23", code)
	}
	assertDockerInvocation(t, executor.invocation, stdin, &stdout, &stderr, []string{
		"--context", "remote-docker", "compose", "ps",
	})
}

func TestRunRuntimeDelegatesDockerInvocationName(t *testing.T) {
	executor := &capturingExecutor{}
	runtime := Runtime{
		ProgramName:   "/usr/local/bin/docker",
		DockerCLIPath: "/bundle/docker-real",
		ContextName:   "remote-docker",
		Executor:      executor,
	}

	code := RunRuntime(
		context.Background(),
		runtime,
		[]string{"ps"},
		io.Discard,
		io.Discard,
	)

	if code != 0 {
		t.Fatalf("RunRuntime() code = %d, want 0", code)
	}
	want := []string{"--context", "remote-docker", "ps"}
	if !reflect.DeepEqual(executor.invocation.Args, want) {
		t.Fatalf("docker args = %#v, want %#v", executor.invocation.Args, want)
	}
}

func TestRunRuntimeRejectsEndpointOverrides(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "long host", args: []string{"docker", "--host", "tcp://other", "ps"}},
		{name: "long host equals", args: []string{"docker", "--host=tcp://other", "ps"}},
		{name: "short host", args: []string{"docker", "-H", "tcp://other", "ps"}},
		{name: "short host attached", args: []string{"docker", "-Htcp://other", "ps"}},
		{name: "context", args: []string{"docker", "--context", "other", "ps"}},
		{name: "context equals", args: []string{"docker", "--context=other", "ps"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := &capturingExecutor{}
			var stderr bytes.Buffer

			code := RunRuntime(context.Background(), Runtime{
				ProgramName:   "remote-docker",
				DockerCLIPath: "/bundle/docker-real",
				Executor:      executor,
			}, tt.args, io.Discard, &stderr)

			if code != 2 {
				t.Fatalf("RunRuntime() code = %d, want 2", code)
			}
			if executor.called {
				t.Fatal("Docker CLI was called for an endpoint override")
			}
			if got := stderr.String(); !strings.Contains(got, "Remote Docker manages the Docker endpoint") {
				t.Fatalf("stderr = %q, want managed endpoint message", got)
			}
		})
	}
}

func TestRealDockerCLIPath(t *testing.T) {
	want := "/usr/local/libexec/remote-docker/docker-real"
	if got := realDockerCLIPath("/usr/local/bin/remote-docker"); got != want {
		t.Fatalf("realDockerCLIPath() = %q, want %q", got, want)
	}
}

func assertDockerInvocation(
	t *testing.T,
	invocation dockercli.Invocation,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	wantArgs []string,
) {
	t.Helper()

	if invocation.Binary != "/bundle/docker-real" {
		t.Fatalf("docker binary = %q, want /bundle/docker-real", invocation.Binary)
	}
	if !reflect.DeepEqual(invocation.Args, wantArgs) {
		t.Fatalf("docker args = %#v, want %#v", invocation.Args, wantArgs)
	}
	if !reflect.DeepEqual(invocation.Env, []string{"TERM=xterm-256color"}) {
		t.Fatalf("docker env = %#v", invocation.Env)
	}
	if invocation.Dir != "/workspace" {
		t.Fatalf("docker dir = %q, want /workspace", invocation.Dir)
	}
	if invocation.Stdin != stdin || invocation.Stdout != stdout || invocation.Stderr != stderr {
		t.Fatal("docker terminal streams were not preserved")
	}
}

type capturingExecutor struct {
	called     bool
	invocation dockercli.Invocation
	err        error
}

func (e *capturingExecutor) Run(_ context.Context, invocation dockercli.Invocation) error {
	e.called = true
	e.invocation = invocation
	return e.err
}

type codedProcessError struct {
	code int
}

func (e codedProcessError) Error() string {
	return "process failed"
}

func (e codedProcessError) ExitCode() int {
	return e.code
}

var _ error = codedProcessError{}
var _ interface{ ExitCode() int } = codedProcessError{}

func TestRunRuntimeReturnsOneForNonProcessError(t *testing.T) {
	executor := &capturingExecutor{err: errors.New("cannot start")}
	code := RunRuntime(context.Background(), Runtime{
		ProgramName:   "docker",
		DockerCLIPath: "/bundle/docker-real",
		Executor:      executor,
	}, []string{"ps"}, io.Discard, io.Discard)

	if code != 1 {
		t.Fatalf("RunRuntime() code = %d, want 1", code)
	}
}
