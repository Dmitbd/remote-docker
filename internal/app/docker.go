package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/Dmitbd/remote-docker/internal/dockercli"
	"github.com/Dmitbd/remote-docker/internal/localapi"
)

const defaultContextName = "remote-docker"

// Runtime contains process-specific dependencies for one CLI invocation.
type Runtime struct {
	ProgramName    string
	ExecutablePath string
	DockerCLIPath  string
	ContextName    string
	Executor       dockercli.Executor
	Preflight      DockerPreflight
	ControlClient  ControlClient
	Env            []string
	Dir            string
	Stdin          io.Reader
}

// DockerPreflight gates one invocation without executing the user's command.
type DockerPreflight interface {
	Check(context.Context, dockercli.Invocation, dockercli.Executor, io.Writer) error
}

// LocalAgentDockerPreflight delegates the production safety gate to the
// owner-only background-agent transport. Environment values and stdin are
// intentionally never serialized.
type LocalAgentDockerPreflight struct {
	Client ControlClient
}

func (p LocalAgentDockerPreflight) Check(
	ctx context.Context,
	invocation dockercli.Invocation,
	real dockercli.Executor,
	_ io.Writer,
) error {
	analysis, err := dockercli.Analyze(ctx, invocation, real)
	if err != nil {
		return fmt.Errorf("analyze Docker command: %w", err)
	}
	params := localapi.PrepareDockerParams{
		BindSources: append([]string(nil), analysis.BindSources...), WorkingDirectory: invocation.Dir,
	}
	for _, port := range analysis.StaticTCPPorts {
		params.StaticTCPPorts = append(params.StaticTCPPorts, localapi.DockerPort{
			HostIP: port.HostIP, HostPort: port.HostPort, ContainerPort: port.ContainerPort,
		})
	}
	for _, reason := range analysis.Unsupported {
		params.Unsupported = append(params.Unsupported, localapi.DockerUnsupported{
			Code: string(reason.Code), Detail: reason.Detail,
		})
	}
	client := p.Client
	if client == nil {
		client = localapi.Client{}
	}
	var result localapi.PrepareDockerResult
	if err := client.Call(ctx, localapi.MethodPrepareDocker, params, &result); err != nil {
		return err
	}
	if !result.Ready {
		return errors.New("background agent did not prepare the Docker invocation")
	}
	return nil
}

func runDocker(ctx context.Context, runtime Runtime, args []string, stdout, stderr io.Writer) int {
	if hasEndpointOverride(args) {
		fmt.Fprintln(stderr, "Remote Docker manages the Docker endpoint; --host, -H, and --context are not allowed")
		return 2
	}

	executor := runtime.Executor
	if executor == nil {
		executor = dockercli.Runner{}
	}
	contextName := runtime.ContextName
	if contextName == "" {
		contextName = defaultContextName
	}
	dockerCLIPath := runtime.DockerCLIPath
	if dockerCLIPath == "" {
		dockerCLIPath = realDockerCLIPath(runtime.ExecutablePath)
	}

	dockerArgs := make([]string, 0, len(args)+2)
	dockerArgs = append(dockerArgs, "--context", contextName)
	dockerArgs = append(dockerArgs, args...)
	invocation := dockercli.Invocation{
		Binary: dockerCLIPath,
		Args:   dockerArgs,
		Env:    runtime.Env,
		Dir:    runtime.Dir,
		Stdin:  runtime.Stdin,
		Stdout: stdout,
		Stderr: stderr,
	}
	if runtime.Preflight != nil {
		if err := runtime.Preflight.Check(ctx, invocation, executor, stderr); err != nil {
			fmt.Fprintf(stderr, "remote-docker: %v\n", err)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.Canceled) {
				return 130
			}
			return 1
		}
	}
	err := executor.Run(ctx, invocation)
	if err != nil && !isProcessExit(err) {
		fmt.Fprintf(stderr, "remote-docker: %v\n", err)
	}

	return dockercli.ExitCode(err)
}

func realDockerCLIPath(executablePath string) string {
	return filepath.Clean(filepath.Join(
		filepath.Dir(executablePath),
		"..",
		"libexec",
		"remote-docker",
		"docker-real",
	))
}

func hasEndpointOverride(args []string) bool {
	for _, argument := range args {
		if argument == "--" || !strings.HasPrefix(argument, "-") {
			return false
		}
		if argument == "--host" ||
			argument == "-H" ||
			argument == "--context" ||
			strings.HasPrefix(argument, "--host=") ||
			strings.HasPrefix(argument, "--context=") ||
			(strings.HasPrefix(argument, "-H") && len(argument) > 2) {
			return true
		}
	}

	return false
}

func isProcessExit(err error) bool {
	var exitError interface{ ExitCode() int }
	return errors.As(err, &exitError)
}
