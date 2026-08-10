package portrelay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/Dmitbd/remote-docker/internal/dockercli"
)

const maxDockerEventSize = 1 << 20

// EventProcess is one background Docker events process.
type EventProcess interface {
	Stdout() io.ReadCloser
	Wait() error
}

// EventStarter starts the infrastructure-only Docker event stream.
type EventStarter interface {
	Start(context.Context, dockercli.Invocation) (EventProcess, error)
}

// DockerSource derives relay desired state through the managed Docker context.
// It issues only container inspection and event-stream commands.
type DockerSource struct {
	CLI          string
	Context      string
	Env          []string
	Executor     dockercli.Executor
	EventStarter EventStarter
}

// RunningContainers returns the complete inspect snapshot for running IDs.
func (s DockerSource) RunningContainers(ctx context.Context) ([]Container, error) {
	cli, contextName, executor, err := s.dependencies()
	if err != nil {
		return nil, err
	}
	var identifiers bytes.Buffer
	if err := executor.Run(ctx, dockercli.Invocation{
		Binary: cli,
		Args: []string{
			"--context", contextName,
			"container", "ls", "--quiet", "--filter", "status=running",
		},
		Env: s.Env, Stdout: &identifiers, Stderr: io.Discard,
	}); err != nil {
		return nil, fmt.Errorf("list running Docker containers: %w", err)
	}
	ids := strings.Fields(identifiers.String())
	if len(ids) == 0 {
		return nil, nil
	}

	var inspected bytes.Buffer
	arguments := []string{"--context", contextName, "inspect"}
	arguments = append(arguments, ids...)
	if err := executor.Run(ctx, dockercli.Invocation{
		Binary: cli, Args: arguments, Env: s.Env,
		Stdout: &inspected, Stderr: io.Discard,
	}); err != nil {
		return nil, fmt.Errorf("inspect running Docker containers: %w", err)
	}
	return DecodeInspect(&inspected)
}

// Events opens a restartable container lifecycle stream.
func (s DockerSource) Events(ctx context.Context) (<-chan Event, error) {
	cli, contextName, _, err := s.dependencies()
	if err != nil {
		return nil, err
	}
	starter := s.EventStarter
	if starter == nil {
		starter = execEventStarter{}
	}
	process, err := starter.Start(ctx, dockercli.Invocation{
		Binary: cli,
		Args: []string{
			"--context", contextName,
			"events", "--format", "{{json .}}", "--filter", "type=container",
		},
		Env: s.Env, Stderr: io.Discard,
	})
	if err != nil {
		return nil, fmt.Errorf("start Docker event stream: %w", err)
	}

	events := make(chan Event)
	go func() {
		defer close(events)
		output := process.Stdout()
		defer output.Close()
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 64<<10), maxDockerEventSize)
		for scanner.Scan() {
			event, decodeErr := DecodeEvent(scanner.Bytes())
			if decodeErr != nil {
				break
			}
			select {
			case events <- event:
			case <-ctx.Done():
				_ = process.Wait()
				return
			}
		}
		_ = process.Wait()
	}()
	return events, nil
}

func (s DockerSource) dependencies() (string, string, dockercli.Executor, error) {
	if strings.TrimSpace(s.CLI) == "" || strings.TrimSpace(s.Context) == "" {
		return "", "", nil, errors.New("Docker relay source configuration is incomplete")
	}
	executor := s.Executor
	if executor == nil {
		executor = dockercli.Runner{}
	}
	return s.CLI, s.Context, executor, nil
}

type execEventStarter struct{}

func (execEventStarter) Start(ctx context.Context, invocation dockercli.Invocation) (EventProcess, error) {
	command := exec.CommandContext(ctx, invocation.Binary, invocation.Args...)
	command.Env = invocation.Env
	command.Dir = invocation.Dir
	command.Stderr = invocation.Stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		_ = stdout.Close()
		return nil, err
	}
	return &execEventProcess{command: command, stdout: stdout}, nil
}

type execEventProcess struct {
	command *exec.Cmd
	stdout  io.ReadCloser
}

func (p *execEventProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *execEventProcess) Wait() error           { return p.command.Wait() }

var _ Source = DockerSource{}
