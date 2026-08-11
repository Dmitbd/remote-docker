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
			{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-a"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-peer"}}}]`},
		},
	}

	change, err := EnsureContext(
		context.Background(),
		executor,
		"docker-real",
		"remote-docker",
		"ssh://remote-docker-device-peer",
		"owner-a",
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
			"--description", "Managed by Remote Docker; owner=owner-a",
			"--docker", "host=ssh://remote-docker-device-peer",
			"remote-docker",
		},
		{"context", "inspect", "remote-docker"},
	}
	if !reflect.DeepEqual(executor.args(), want) {
		t.Fatalf("commands = %#v, want %#v", executor.args(), want)
	}
}

func TestPlanAndApplyContextSeparateObservationFromMutation(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{
		{err: codedError{code: 1}},
		{err: codedError{code: 1}},
		{},
		{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-a"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-peer"}}}]`},
	}}
	change, err := PlanContext(
		context.Background(), executor, "docker-real", "remote-docker", "ssh://remote-docker-device-peer", "owner-a",
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
		{"context", "create", "--description", "Managed by Remote Docker; owner=owner-a", "--docker", "host=ssh://remote-docker-device-peer", "remote-docker"},
		{"context", "inspect", "remote-docker"},
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
	change, err := PlanContext(context.Background(), executor, "docker-real", "remote-docker", "ssh://remote-docker-device-peer", "owner-a")
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
	change, err := PlanContext(context.Background(), executor, "docker-real", "remote-docker", "ssh://remote-docker-device-peer", "owner-a")
	if err != nil {
		t.Fatalf("PlanContext() error = %v", err)
	}
	err = ApplyContext(context.Background(), executor, "docker-real", change)
	if !errors.Is(err, ErrContextOwnershipLost) {
		t.Fatalf("ApplyContext() error = %v, want ownership lost", err)
	}
	want := [][]string{
		{"context", "inspect", "remote-docker"},
		{"context", "inspect", "remote-docker"},
		{"context", "create", "--description", "Managed by Remote Docker; owner=owner-a", "--docker", "host=ssh://remote-docker-device-peer", "remote-docker"},
		{"context", "inspect", "remote-docker"},
	}
	if got := executor.args(); !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %#v, want ownership check after failed create", got)
	}
}

func TestApplyContextUsesIndependentVerificationAfterCancelledCreate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	executor := &cancelledCreateExecutor{cancel: cancel}
	change, err := PlanContext(ctx, executor, "docker-real", "remote-docker", "ssh://remote-docker-device-peer", "owner-a")
	if err != nil {
		t.Fatalf("PlanContext() error = %v", err)
	}
	err = ApplyContext(ctx, executor, "docker-real", change)
	if !errors.Is(err, ErrContextResultUnknown) {
		t.Fatalf("ApplyContext() error = %v, want unknown result", err)
	}
	if executor.verificationContextErr != nil {
		t.Fatalf("verification inherited cancelled create context: %v", executor.verificationContextErr)
	}
	if executor.calls != 4 {
		t.Fatalf("executor calls = %d, want plan, precondition, create, independent inspect", executor.calls)
	}
}

func TestApplyContextKeepsMalformedVerificationAsUnknown(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{
		{err: codedError{code: 1}},
		{err: codedError{code: 1}},
		{},
		{stdout: `{not-json`},
	}}
	change, err := PlanContext(context.Background(), executor, "docker-real", "remote-docker", "ssh://remote-docker-device-peer", "owner-a")
	if err != nil {
		t.Fatalf("PlanContext() error = %v", err)
	}
	err = ApplyContext(context.Background(), executor, "docker-real", change)
	if !errors.Is(err, ErrContextResultUnknown) {
		t.Fatalf("ApplyContext() error = %v, want unknown result", err)
	}
}

func TestApplyContextRejectsManagedEndpointChangedAfterPlan(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{
		{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-old"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-old"}}}]`},
		{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-old"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-other"}}}]`},
	}}
	change, err := PlanContext(
		context.Background(), executor, "docker-real", "remote-docker", "ssh://remote-docker-device-new",
		"owner-new", ContextChange{Name: "remote-docker", CurrentHost: "ssh://remote-docker-device-old", OwnerToken: "owner-old"},
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

func TestPlanContextRejectsLegacyManagedContext(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-peer"}}}]`}}}

	_, err := PlanContext(
		context.Background(), executor, "docker-real", "remote-docker", "ssh://remote-docker-device-peer", "owner-a",
	)
	if !errors.Is(err, ErrContextCollision) {
		t.Fatalf("PlanContext() error = %v, want legacy ownership collision", err)
	}
	if got := executor.args(); !reflect.DeepEqual(got, [][]string{{"context", "inspect", "remote-docker"}}) {
		t.Fatalf("legacy context was mutated: %#v", got)
	}
}

func TestRestoreContextRevertsOnlyExpectedManagedEndpoint(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{
		{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-new"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-new"}}}]`},
		{},
		{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-old"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-old"}}}]`},
	}}
	change := ContextChange{
		Name: "remote-docker", PreviousHost: "ssh://remote-docker-device-old", PreviousDescription: "Managed by Remote Docker; owner=owner-old",
		CurrentHost: "ssh://remote-docker-device-new", OwnerToken: "owner-new",
	}

	if err := RestoreContext(context.Background(), executor, "docker-real", change); err != nil {
		t.Fatalf("RestoreContext() error = %v", err)
	}
	want := [][]string{
		{"context", "inspect", "remote-docker"},
		{"context", "update", "--description", "Managed by Remote Docker; owner=owner-old", "--docker", "host=ssh://remote-docker-device-old", "remote-docker"},
		{"context", "inspect", "remote-docker"},
	}
	if !reflect.DeepEqual(executor.args(), want) {
		t.Fatalf("commands = %#v, want %#v", executor.args(), want)
	}
}

func TestRestoreContextTreatsDifferentOwnerTokenAsOwnershipLost(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-b"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-peer"}}}]`}}}
	change := ContextChange{
		Name: "remote-docker", CurrentHost: "ssh://remote-docker-device-peer", Created: true, OwnerToken: "owner-a",
	}

	err := RestoreContext(context.Background(), executor, "docker-real", change)
	if !errors.Is(err, ErrContextOwnershipLost) {
		t.Fatalf("RestoreContext() error = %v, want ownership lost", err)
	}
	if got := executor.args(); !reflect.DeepEqual(got, [][]string{{"context", "inspect", "remote-docker"}}) {
		t.Fatalf("foreign context was mutated: %#v", got)
	}
}

func TestRestoreContextTreatsLegacyMetadataAsOwnershipLost(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-peer"}}}]`}}}
	change := ContextChange{
		Name: "remote-docker", CurrentHost: "ssh://remote-docker-device-peer", Created: true, OwnerToken: "owner-a",
	}

	err := RestoreContext(context.Background(), executor, "docker-real", change)
	if !errors.Is(err, ErrContextOwnershipLost) {
		t.Fatalf("RestoreContext() error = %v, want ownership lost", err)
	}
	if got := executor.args(); !reflect.DeepEqual(got, [][]string{{"context", "inspect", "remote-docker"}}) {
		t.Fatalf("legacy context was mutated: %#v", got)
	}
}

func TestRestoreContextRemovesOnlyExactOwnerAndVerifiesResult(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{
		{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-a"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-peer"}}}]`},
		{},
		{err: codedError{code: 1}},
	}}
	change := ContextChange{
		Name: "remote-docker", CurrentHost: "ssh://remote-docker-device-peer", Created: true, OwnerToken: "owner-a",
	}

	if err := RestoreContext(context.Background(), executor, "docker-real", change); err != nil {
		t.Fatalf("RestoreContext() error = %v", err)
	}
	want := [][]string{
		{"context", "inspect", "remote-docker"},
		{"context", "rm", "--force", "remote-docker"},
		{"context", "inspect", "remote-docker"},
	}
	if got := executor.args(); !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %#v, want exact-owner remove plus result verification", got)
	}
}

func TestRestoreContextAcceptsVerifiedRemovalAfterCommandError(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{
		{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-a"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-peer"}}}]`},
		{err: context.Canceled},
		{err: codedError{code: 1}},
	}}
	change := ContextChange{
		Name: "remote-docker", CurrentHost: "ssh://remote-docker-device-peer", Created: true, OwnerToken: "owner-a",
	}

	if err := RestoreContext(context.Background(), executor, "docker-real", change); err != nil {
		t.Fatalf("RestoreContext() error = %v, want verified removal", err)
	}
}

func TestRestoreContextKeepsUnknownRemovalResultForRetry(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{
		{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-a"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-peer"}}}]`},
		{err: context.Canceled},
		{err: codedError{code: 2}},
	}}
	change := ContextChange{
		Name: "remote-docker", CurrentHost: "ssh://remote-docker-device-peer", Created: true, OwnerToken: "owner-a",
	}

	err := RestoreContext(context.Background(), executor, "docker-real", change)
	if !errors.Is(err, ErrContextResultUnknown) {
		t.Fatalf("RestoreContext() error = %v, want unknown result", err)
	}
}

func TestRestoreContextIsIdempotentAfterCreatedContextWasRemoved(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{{err: codedError{code: 1}}}}
	change := ContextChange{
		Name: "remote-docker", CurrentHost: "ssh://remote-docker-device-peer", Created: true, OwnerToken: "owner-a",
	}
	if err := RestoreContext(context.Background(), executor, "docker-real", change); err != nil {
		t.Fatalf("RestoreContext() error = %v", err)
	}
	if got := executor.args(); !reflect.DeepEqual(got, [][]string{{"context", "inspect", "remote-docker"}}) {
		t.Fatalf("commands = %#v", got)
	}
}

func TestRestoreContextIsIdempotentAfterPreviousEndpointWasRestored(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-old"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-old"}}}]`}}}
	change := ContextChange{
		Name: "remote-docker", PreviousHost: "ssh://remote-docker-device-old", PreviousDescription: "Managed by Remote Docker; owner=owner-old",
		CurrentHost: "ssh://remote-docker-device-new", OwnerToken: "owner-new",
	}
	if err := RestoreContext(context.Background(), executor, "docker-real", change); err != nil {
		t.Fatalf("RestoreContext() error = %v", err)
	}
	if got := executor.args(); !reflect.DeepEqual(got, [][]string{{"context", "inspect", "remote-docker"}}) {
		t.Fatalf("commands = %#v", got)
	}
}

func TestRestoreContextAcceptsVerifiedUpdateAfterCommandError(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{
		{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-new"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-new"}}}]`},
		{err: context.Canceled},
		{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-old"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-old"}}}]`},
	}}
	change := ContextChange{
		Name: "remote-docker", PreviousHost: "ssh://remote-docker-device-old", PreviousDescription: "Managed by Remote Docker; owner=owner-old",
		CurrentHost: "ssh://remote-docker-device-new", OwnerToken: "owner-new",
	}

	if err := RestoreContext(context.Background(), executor, "docker-real", change); err != nil {
		t.Fatalf("RestoreContext() error = %v, want verified update", err)
	}
}

func TestRestoreContextRejectsPreviousEndpointWithDifferentDescription(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-other"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-old"}}}]`}}}
	change := ContextChange{
		Name: "remote-docker", PreviousHost: "ssh://remote-docker-device-old", PreviousDescription: "Managed by Remote Docker; owner=owner-old",
		CurrentHost: "ssh://remote-docker-device-new", OwnerToken: "owner-new",
	}

	err := RestoreContext(context.Background(), executor, "docker-real", change)
	if !errors.Is(err, ErrContextOwnershipLost) {
		t.Fatalf("RestoreContext() error = %v, want ownership lost", err)
	}
	if got := executor.args(); !reflect.DeepEqual(got, [][]string{{"context", "inspect", "remote-docker"}}) {
		t.Fatalf("context with mismatched description was mutated: %#v", got)
	}
}

func TestEnsureContextKeepsMatchingManagedContext(t *testing.T) {
	executor := &recordingExecutor{
		results: []executorResult{{stdout: `[
  {
    "Name": "remote-docker",
    "Metadata": {"Description": "Managed by Remote Docker; owner=owner-a"},
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
		"owner-a",
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
			{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-old"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-old"}}}]`},
			{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-old"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-old"}}}]`},
			{},
			{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-new"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-new"}}}]`},
		},
	}

	change, err := EnsureContext(
		context.Background(), executor, "docker-real", "remote-docker", "ssh://remote-docker-device-new",
		"owner-new", ContextChange{Name: "remote-docker", CurrentHost: "ssh://remote-docker-device-old", OwnerToken: "owner-old"},
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
		{"context", "update", "--description", "Managed by Remote Docker; owner=owner-new", "--docker", "host=ssh://remote-docker-device-new", "remote-docker"},
		{"context", "inspect", "remote-docker"},
	}
	if !reflect.DeepEqual(executor.args(), want) {
		t.Fatalf("commands = %#v, want %#v", executor.args(), want)
	}
}

func TestEnsureContextRejectsDifferingManagedEndpointWithoutExactPreviousHost(t *testing.T) {
	executor := &recordingExecutor{results: []executorResult{{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-old"}}}]`}}}

	_, err := EnsureContext(
		context.Background(), executor, "docker-real", "remote-docker", "ssh://remote-docker-device-new",
		"owner-new",
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
				"owner-a",
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
	executor := &recordingExecutor{results: []executorResult{{stdout: `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker; owner=owner-other"},"Endpoints":{"docker":{"Host":"ssh://remote-docker-device-other"}}}]`}}}

	_, err := EnsureContext(
		context.Background(), executor, "docker-real", "remote-docker",
		"ssh://remote-docker-device-new", "owner-new",
		ContextChange{Name: "remote-docker", CurrentHost: "ssh://remote-docker-device-expected", OwnerToken: "owner-expected"},
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
		"owner-a",
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

type cancelledCreateExecutor struct {
	cancel                 context.CancelFunc
	calls                  int
	verificationContextErr error
}

func (e *cancelledCreateExecutor) Run(ctx context.Context, _ Invocation) error {
	e.calls++
	switch e.calls {
	case 1, 2:
		return codedError{code: 1}
	case 3:
		e.cancel()
		return context.Canceled
	default:
		e.verificationContextErr = ctx.Err()
		return codedError{code: 2}
	}
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
