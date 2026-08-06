package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/Dmitbd/remote-docker/internal/dockercli"
)

const defaultContextName = "remote-docker"

// Runtime contains process-specific dependencies for one CLI invocation.
type Runtime struct {
	ProgramName    string
	ExecutablePath string
	DockerCLIPath  string
	ContextName    string
	Executor       dockercli.Executor
	Preflight      *Preflight
	Env            []string
	Dir            string
	Stdin          io.Reader
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
