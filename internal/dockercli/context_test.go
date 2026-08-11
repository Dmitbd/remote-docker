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
			{err: codedError{code: 1}},
			{},
		},
	}

	change, err := EnsureContext(
		context.Background(),
		executor,
		"docker-real",
		"remote-docker",
		"ssh://remote-docker-device-peer",
	)
	if err != nil {
		t.Fatalf("EnsureContext() error = %v", err)
	}
	if !change.Created || !change.Changed() {
		t.Fatalf("EnsureContext() change = %#v, want created context", change)
	}

	want := [][]string{
		{"context", "inspect", "remote-docker"},
		{"context", "inspect", "remote-docker"},
		{
			"context", "create",
			"--description", managedContextDescription,
			"--docker", "host=ssh://remote-docker-device-peer",
			"remote-docker",
		},
	}
	if !reflect.DeepEqual(executor.args(), want) {
		t.Fatalf("commands = %#v, want %#v", executor.args(), want)
	}
}

func TestPlanAndApplyContextSeparateObservationFromMutation(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{{err: codedError{code: 1}}, {err: codedError{code: 1}}, {}}}
	change, err := PlanContext(
		context.Background(), executor, "docker-real", "remote-docker", "ssh://remote-docker-device-peer",
	)
	if err != nil {
		t.Fatalf("PlanContext() error = %v", err)
	}
	if !change.Created || !reflect.DeepEqual(executor.args(), [][]string{{"context", "inspect", "remote-docker"}}) {
		t.Fatalf("plan change=%#v commands=%#v", change, executor.args())
	}
	if err := ApplyContext(context.Background(), executor, "docker-real", change); err != nil {
		t.Fatalf("ApplyContext() error = %v", err)
	}
	want := [][]string{
		{"context", "inspect", "remote-docker"},
		{"context", "inspect", "remote-docker"},
		{"context", "create", "--description", managedContextDescription, "--docker", "host=ssh://remote-docker-device-peer", "remote-docker"},
	}
	if !reflect.DeepEqual(executor.args(), want) {
		t.Fatalf("commands = %#v, want %#v", executor.args(), want)
	}
}

func TestApplyContextRejectsContextCreatedAfterPlan(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{
		{err: codedError{code: 1}},
		{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-other"}}}]`},
	}}
	change, err := PlanContext(context.Background(), executor, "docker-real", "remote-docker", "ssh://remote-docker-device-peer")
	if err != nil {
		t.Fatalf("PlanContext() error = %v", err)
	}
	err = ApplyContext(context.Background(), executor, "docker-real", change)
	if !errors.Is(err, ErrContextCollision) {
		t.Fatalf("ApplyContext() error = %v, want collision", err)
	}
	want := [][]string{{"context", "inspect", "remote-docker"}, {"context", "inspect", "remote-docker"}}
	if got := executor.args(); !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %#v, want no mutation after changed precondition", got)
	}
}

func TestApplyContextDoesNotClaimRollbackOwnershipAfterFailedCreateRace(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{
		{err: codedError{code: 1}},
		{err: codedError{code: 1}},
		{err: codedError{code: 1}},
		{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-other"}}}]`},
	}}
	change, err := PlanContext(context.Background(), executor, "docker-real", "remote-docker", "ssh://remote-docker-device-peer")
	if err != nil {
		t.Fatalf("PlanContext() error = %v", err)
	}
	err = ApplyContext(context.Background(), executor, "docker-real", change)
	if !errors.Is(err, ErrContextPrecondition) || !errors.Is(err, ErrContextCollision) {
		t.Fatalf("ApplyContext() error = %v, want ambiguous precondition collision", err)
	}
	want := [][]string{
		{"context", "inspect", "remote-docker"},
		{"context", "inspect", "remote-docker"},
		{"context", "create", "--description", managedContextDescription, "--docker", "host=ssh://remote-docker-device-peer", "remote-docker"},
		{"context", "inspect", "remote-docker"},
	}
	if got := executor.args(); !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %#v, want ownership check after failed create", got)
	}
}

func TestApplyContextRejectsManagedEndpointChangedAfterPlan(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{
		{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-old"}}}]`},
		{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-other"}}}]`},
	}}
	change, err := PlanContext(
		context.Background(), executor, "docker-real", "remote-docker", "ssh://remote-docker-device-new",
		"ssh://remote-docker-device-old",
	)
	if err != nil {
		t.Fatalf("PlanContext() error = %v", err)
	}
	err = ApplyContext(context.Background(), executor, "docker-real", change)
	if !errors.Is(err, ErrContextCollision) {
		t.Fatalf("ApplyContext() error = %v, want collision", err)
	}
	want := [][]string{{"context", "inspect", "remote-docker"}, {"context", "inspect", "remote-docker"}}
	if got := executor.args(); !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %#v, want no mutation after changed precondition", got)
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

func TestRestoreContextIsIdempotentAfterCreatedContextWasRemoved(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{{err: codedError{code: 1}}}}
	change := ContextChange{
		Name: "remote-docker", CurrentHost: "ssh://remote-docker-device-peer", Created: true,
	}
	if err := RestoreContext(context.Background(), executor, "docker-real", change); err != nil {
		t.Fatalf("RestoreContext() error = %v", err)
	}
	if got := executor.args(); !reflect.DeepEqual(got, [][]string{{"context", "inspect", "remote-docker"}}) {
		t.Fatalf("commands = %#v", got)
	}
}

func TestRestoreContextIsIdempotentAfterPreviousEndpointWasRestored(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-old"}}}]`}}}
	change := ContextChange{
		Name: "remote-docker", PreviousHost: "ssh://remote-docker-device-old", CurrentHost: "ssh://remote-docker-device-new",
	}
	if err := RestoreContext(context.Background(), executor, "docker-real", change); err != nil {
		t.Fatalf("RestoreContext() error = %v", err)
	}
	if got := executor.args(); !reflect.DeepEqual(got, [][]string{{"context", "inspect", "remote-docker"}}) {
		t.Fatalf("commands = %#v", got)
	}
}

func TestEnsureContextKeepsMatchingManagedContext(t *testing.T) {
	executor := &recordingExecutor{
		results: []executorResult{{stdout: `[
  {
    "Name": "remote-docker",
    "Metadata": {"Description": "Managed by Remote Docker"},
    "Endpoints": {"docker": {"Host": "ssh://remote-docker-device-peer"}}
  }
]`}},
	}

	change, err := EnsureContext(
		context.Background(),
		executor,
		"docker-real",
		"remote-docker",
		"ssh://remote-docker-device-peer",
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
			{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-old"}}}]`},
			{},
		},
	}

	change, err := EnsureContext(
		context.Background(), executor, "docker-real", "remote-docker", "ssh://remote-docker-device-new",
		"ssh://remote-docker-device-old",
	)
	if err != nil {
		t.Fatalf("EnsureContext() error = %v", err)
	}
	if change.PreviousHost != "ssh://remote-docker-device-old" || change.CurrentHost != "ssh://remote-docker-device-new" {
		t.Fatalf("EnsureContext() change = %#v", change)
	}
	want := [][]string{
		{"context", "inspect", "remote-docker"},
		{"context", "inspect", "remote-docker"},
		{"context", "update", "--docker", "host=ssh://remote-docker-device-new", "remote-docker"},
	}
	if !reflect.DeepEqual(executor.args(), want) {
		t.Fatalf("commands = %#v, want %#v", executor.args(), want)
	}
}

func TestEnsureContextRejectsDifferingManagedEndpointWithoutExactPreviousHost(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-old"}}}]`}}}

	_, err := EnsureContext(
		context.Background(), executor, "docker-real", "remote-docker", "ssh://remote-docker-device-new",
	)
	if !errors.Is(err, ErrContextCollision) {
		t.Fatalf("EnsureContext() error = %v, want ErrContextCollision", err)
	}
	if got := executor.args(); !reflect.DeepEqual(got, [][]string{{"context", "inspect", "remote-docker"}}) {
		t.Fatalf("unowned managed endpoint was mutated: %#v", got)
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
  "Endpoints": {"docker": {"Host": "ssh://remote-docker-device-peer"}}
}]`,
		},
		{
			name:   "different exact name",
			stdout: `[{"Name":"remote-docker-other","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-peer"}}}]`,
		},
		{
			name:   "same description foreign endpoint",
			stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":"ssh://user@foreign-host"}}}]`,
		},
		{
			name:   "same description empty endpoint",
			stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":""}}}]`,
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
				"ssh://remote-docker-device-peer",
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

func TestEnsureContextRejectsUnexpectedPreviousManagedEndpoint(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-other"}}}]`}}}

	_, err := EnsureContext(
		context.Background(), executor, "docker-real", "remote-docker",
		"ssh://remote-docker-device-new", "ssh://remote-docker-device-expected",
	)
	if !errors.Is(err, ErrContextCollision) {
		t.Fatalf("EnsureContext() error = %v, want ErrContextCollision", err)
	}
	if got := executor.args(); !reflect.DeepEqual(got, [][]string{{"context", "inspect", "remote-docker"}}) {
		t.Fatalf("unexpected previous endpoint was mutated: %#v", got)
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
		"ssh://remote-docker-device-peer",
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
