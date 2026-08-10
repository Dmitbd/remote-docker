package dockercli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
)

func TestEnsureContextCreatesMissingManagedContext(t *testing.T) {
	executor := &recordingExecutor{
		results: []executorResult{
			{err: codedError{code: 1}},
			{},
		},
	}

	change, err := EnsureContext(
		context.Background(),
		executor,
		"docker-real",
		"remote-docker",
		"ssh://remote-docker@remote-host",
	)
	if err != nil {
		t.Fatalf("EnsureContext() error = %v", err)
	}
	if !change.Created || !change.Changed() {
		t.Fatalf("EnsureContext() change = %#v, want created context", change)
	}

	want := [][]string{
		{"context", "inspect", "remote-docker"},
		{
			"context", "create",
			"--description", managedContextDescription,
			"--docker", "host=ssh://remote-docker@remote-host",
			"remote-docker",
		},
	}
	if !reflect.DeepEqual(executor.args(), want) {
		t.Fatalf("commands = %#v, want %#v", executor.args(), want)
	}
}

func TestRestoreContextRevertsOnlyExpectedManagedEndpoint(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{
		{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-new"}}}]`},
		{},
	}}
	change := ContextChange{
		Name: "remote-docker", PreviousHost: "ssh://remote-docker-device-old", CurrentHost: "ssh://remote-docker-device-new",
	}

	if err := RestoreContext(context.Background(), executor, "docker-real", change); err != nil {
		t.Fatalf("RestoreContext() error = %v", err)
	}
	want := [][]string{
		{"context", "inspect", "remote-docker"},
		{"context", "update", "--docker", "host=ssh://remote-docker-device-old", "remote-docker"},
	}
	if !reflect.DeepEqual(executor.args(), want) {
		t.Fatalf("commands = %#v, want %#v", executor.args(), want)
	}
}

func TestEnsureContextKeepsMatchingManagedContext(t *testing.T) {
	executor := &recordingExecutor{
		results: []executorResult{{stdout: `[
  {
    "Name": "remote-docker",
    "Metadata": {"Description": "Managed by Remote Docker"},
    "Endpoints": {"docker": {"Host": "ssh://remote-docker@remote-host"}}
  }
]`}},
	}

	change, err := EnsureContext(
		context.Background(),
		executor,
		"docker-real",
		"remote-docker",
		"ssh://remote-docker@remote-host",
	)
	if err != nil {
		t.Fatalf("EnsureContext() error = %v", err)
	}
	if change.Changed() {
		t.Fatalf("EnsureContext() change = %#v, want no change", change)
	}

	want := [][]string{{"context", "inspect", "remote-docker"}}
	if !reflect.DeepEqual(executor.args(), want) {
		t.Fatalf("commands = %#v, want %#v", executor.args(), want)
	}
}

func TestEnsureContextUpdatesExistingManagedEndpoint(t *testing.T) {
	executor := &recordingExecutor{
		results: []executorResult{
			{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-old"}}}]`},
			{},
		},
	}

	change, err := EnsureContext(
		context.Background(), executor, "docker-real", "remote-docker", "ssh://remote-docker-device-new",
	)
	if err != nil {
		t.Fatalf("EnsureContext() error = %v", err)
	}
	if change.PreviousHost != "ssh://remote-docker-device-old" || change.CurrentHost != "ssh://remote-docker-device-new" {
		t.Fatalf("EnsureContext() change = %#v", change)
	}
	want := [][]string{
		{"context", "inspect", "remote-docker"},
		{"context", "update", "--docker", "host=ssh://remote-docker-device-new", "remote-docker"},
	}
	if !reflect.DeepEqual(executor.args(), want) {
		t.Fatalf("commands = %#v, want %#v", executor.args(), want)
	}
}

func TestEnsureContextRejectsContextCollision(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
	}{
		{
			name: "different description",
			stdout: `[{
  "Name": "remote-docker",
  "Metadata": {"Description": "Created by user"},
  "Endpoints": {"docker": {"Host": "ssh://remote-docker@remote-host"}}
}]`,
		},
		{
			name:   "different exact name",
			stdout: `[{"Name":"remote-docker-other","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":"ssh://remote-docker@remote-host"}}}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := &recordingExecutor{
				results: []executorResult{{stdout: tt.stdout}},
			}

			_, err := EnsureContext(
				context.Background(),
				executor,
				"docker-real",
				"remote-docker",
				"ssh://remote-docker@remote-host",
			)

			if !errors.Is(err, ErrContextCollision) {
				t.Fatalf("EnsureContext() error = %v, want ErrContextCollision", err)
			}
			want := [][]string{{"context", "inspect", "remote-docker"}}
			if !reflect.DeepEqual(executor.args(), want) {
				t.Fatalf("commands = %#v, want %#v", executor.args(), want)
			}
		})
	}
}

func TestEnsureContextDoesNotCreateAfterUnexpectedInspectFailure(t *testing.T) {
	executor := &recordingExecutor{
		results: []executorResult{{err: codedError{code: 2}}},
	}

	_, err := EnsureContext(
		context.Background(),
		executor,
		"docker-real",
		"remote-docker",
		"ssh://remote-docker@remote-host",
	)

	if err == nil {
		t.Fatal("EnsureContext() error = nil, want inspect failure")
	}
	want := [][]string{{"context", "inspect", "remote-docker"}}
	if !reflect.DeepEqual(executor.args(), want) {
		t.Fatalf("commands = %#v, want %#v", executor.args(), want)
	}
}

type recordingExecutor struct {
	invocations []Invocation
	results     []executorResult
}

func (e *recordingExecutor) Run(_ context.Context, invocation Invocation) error {
	e.invocations = append(e.invocations, invocation)
	result := e.results[len(e.invocations)-1]
	if result.stdout != "" {
		_, _ = io.WriteString(invocation.Stdout, result.stdout)
	}
	return result.err
}

func (e *recordingExecutor) args() [][]string {
	commands := make([][]string, 0, len(e.invocations))
	for _, invocation := range e.invocations {
		if invocation.Binary != "docker-real" {
			commands = append(commands, []string{"unexpected-binary:" + invocation.Binary})
			continue
		}
		commands = append(commands, invocation.Args)
	}
	return commands
}

type executorResult struct {
	stdout string
	err    error
}

type codedError struct {
	code int
}

func (e codedError) Error() string {
	return fmt.Sprintf("exit code %d", e.code)
}

func (e codedError) ExitCode() int {
	return e.code
}
