package dockercli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	managedContextDescription = "Managed by Remote Docker"
	managedOwnerPrefix        = managedContextDescription + "; owner="
)

// ErrContextCollision means the managed context name belongs to another endpoint.
var ErrContextCollision = errors.New("docker context name collision")

// ErrContextPrecondition means Docker context state changed after PlanContext,
// before ApplyContext issued its mutation command.
var ErrContextPrecondition = errors.New("docker context changed after plan")

// ErrContextOwnershipLost means the named context no longer carries the exact
// Remote Docker owner token recorded by the rollback journal.
var ErrContextOwnershipLost = errors.New("docker context ownership lost")

// ErrContextResultUnknown means Docker returned but the resulting context
// state could not be verified independently. The durable journal must remain.
var ErrContextResultUnknown = errors.New("docker context result is unknown")

// Executor runs a child-process invocation.
type Executor interface {
	Run(context.Context, Invocation) error
}

type inspectedContext struct {
	Name     string `json:"Name"`
	Metadata struct {
		Description string `json:"Description"`
	} `json:"Metadata"`
	Endpoints struct {
		Docker struct {
			Host string `json:"Host"`
		} `json:"docker"`
	} `json:"Endpoints"`
}

// ContextChange records an ownership-checked managed context mutation so a
// caller can restore the previous endpoint if a later local commit fails.
type ContextChange struct {
	Name                string
	PreviousHost        string
	PreviousDescription string
	CurrentHost         string
	OwnerToken          string
	Created             bool
}

// Changed reports whether EnsureContext mutated Docker CLI state.
func (c ContextChange) Changed() bool {
	if c.Name == "" {
		return false
	}
	return c.Created || c.PreviousHost != c.CurrentHost || c.PreviousDescription != ownedContextDescription(c.OwnerToken)
}

// EnsureContext creates the managed context or verifies its ownership.
func EnsureContext(
	ctx context.Context,
	executor Executor,
	cli string,
	name string,
	host string,
	ownerToken string,
	expectedPrevious ...ContextChange,
) (ContextChange, error) {
	change, err := PlanContext(ctx, executor, cli, name, host, ownerToken, expectedPrevious...)
	if err != nil {
		return ContextChange{}, err
	}
	if err := ApplyContext(ctx, executor, cli, change); err != nil {
		return ContextChange{}, err
	}
	return change, nil
}

// PlanContext observes the managed context and returns the exact mutation and
// rollback record without changing Docker state.
func PlanContext(
	ctx context.Context,
	executor Executor,
	cli string,
	name string,
	host string,
	ownerToken string,
	expectedPrevious ...ContextChange,
) (ContextChange, error) {
	if !managedEndpoint(host) || !validOwnerToken(ownerToken) {
		return ContextChange{}, fmt.Errorf("%w: requested endpoint is not managed by Remote Docker", ErrContextCollision)
	}
	change := ContextChange{Name: name, CurrentHost: host, OwnerToken: ownerToken}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	inspectErr := executor.Run(ctx, Invocation{
		Binary: cli,
		Args:   []string{"context", "inspect", name},
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if inspectErr == nil {
		previousHost, previousDescription, previousOwner, err := managedContextState(stdout.Bytes(), name)
		if err != nil {
			return ContextChange{}, err
		}
		if previousHost == host && previousOwner == ownerToken && previousDescription == ownedContextDescription(ownerToken) {
			change.PreviousHost = previousHost
			change.PreviousDescription = previousDescription
			return change, nil
		}
		if len(expectedPrevious) != 1 ||
			expectedPrevious[0].Name != name ||
			expectedPrevious[0].CurrentHost != previousHost ||
			expectedPrevious[0].OwnerToken != previousOwner ||
			ownedContextDescription(expectedPrevious[0].OwnerToken) != previousDescription {
			return ContextChange{}, fmt.Errorf("%w: %q does not match the exact previous ownership record", ErrContextCollision, name)
		}
		change.PreviousHost = previousHost
		change.PreviousDescription = previousDescription
		return change, nil
	}
	if ExitCode(inspectErr) != 1 {
		return ContextChange{}, fmt.Errorf("inspect docker context %q: %w", name, inspectErr)
	}

	change.Created = true
	return change, nil
}

// ApplyContext applies a previously observed plan. Callers can durably save
// the ContextChange before invoking this mutation.
func ApplyContext(ctx context.Context, executor Executor, cli string, change ContextChange) error {
	if change.Name == "" || !managedEndpoint(change.CurrentHost) || !validOwnerToken(change.OwnerToken) || change.Created && (change.PreviousHost != "" || change.PreviousDescription != "") {
		return fmt.Errorf("%w: managed Docker context plan is invalid", ErrContextCollision)
	}
	if !change.Changed() {
		return nil
	}
	if err := verifyContextPrecondition(ctx, executor, cli, change); err != nil {
		return err
	}
	if !change.Created {
		commandErr := updateContext(ctx, executor, cli, change.Name, change.CurrentHost, ownedContextDescription(change.OwnerToken))
		return verifyAppliedContext(ctx, executor, cli, change, commandErr)
	}
	var stderr bytes.Buffer
	createErr := executor.Run(ctx, Invocation{
		Binary: cli,
		Args: []string{
			"context", "create",
			"--description", ownedContextDescription(change.OwnerToken),
			"--docker", "host=" + change.CurrentHost,
			change.Name,
		},
		Stdout: io.Discard,
		Stderr: &stderr,
	})
	if createErr != nil {
		return verifyAppliedContext(ctx, executor, cli, change, fmt.Errorf("create docker context %q: %w", change.Name, createErr))
	}
	return verifyAppliedContext(ctx, executor, cli, change, nil)
}

func verifyContextPrecondition(ctx context.Context, executor Executor, cli string, change ContextChange) error {
	var stdout bytes.Buffer
	inspectErr := executor.Run(ctx, Invocation{
		Binary: cli,
		Args:   []string{"context", "inspect", change.Name},
		Stdout: &stdout,
		Stderr: io.Discard,
	})
	if change.Created {
		if ExitCode(inspectErr) == 1 {
			return nil
		}
		if inspectErr != nil {
			return fmt.Errorf("verify docker context %q before create: %w", change.Name, inspectErr)
		}
		return fmt.Errorf("%w: %w: %q was created after planning", ErrContextPrecondition, ErrContextCollision, change.Name)
	}
	if inspectErr != nil {
		return fmt.Errorf("%w: %w: inspect %q before update: %v", ErrContextPrecondition, ErrContextCollision, change.Name, inspectErr)
	}
	currentHost, currentDescription, _, err := managedContextState(stdout.Bytes(), change.Name)
	if err != nil || currentHost != change.PreviousHost || currentDescription != change.PreviousDescription {
		return fmt.Errorf("%w: %w: %q changed after planning", ErrContextPrecondition, ErrContextCollision, change.Name)
	}
	return nil
}

func verifyAppliedContext(ctx context.Context, executor Executor, cli string, change ContextChange, commandErr error) error {
	verifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	inspectErr := executor.Run(verifyCtx, Invocation{
		Binary: cli,
		Args:   []string{"context", "inspect", change.Name},
		Stdout: &stdout,
		Stderr: io.Discard,
	})
	if inspectErr != nil {
		if commandErr != nil && ExitCode(inspectErr) == 1 {
			return commandErr
		}
		return fmt.Errorf("%w: verify %q after Docker command: %v", ErrContextResultUnknown, change.Name, inspectErr)
	}
	host, description, owner, err := managedContextState(stdout.Bytes(), change.Name)
	if err != nil {
		if errors.Is(err, ErrContextCollision) {
			return fmt.Errorf("%w: %q does not match the applied ownership record", ErrContextOwnershipLost, change.Name)
		}
		return fmt.Errorf("%w: verify %q ownership metadata: %v", ErrContextResultUnknown, change.Name, err)
	}
	if host != change.CurrentHost || description != ownedContextDescription(change.OwnerToken) || owner != change.OwnerToken {
		return fmt.Errorf("%w: %q does not match the applied ownership record", ErrContextOwnershipLost, change.Name)
	}
	return nil
}

// RestoreContext rolls back a change only while the exact managed context
// still points to the endpoint written by EnsureContext.
func RestoreContext(ctx context.Context, executor Executor, cli string, change ContextChange) error {
	if !change.Changed() {
		return nil
	}
	if change.Name == "" || !managedEndpoint(change.CurrentHost) || !validOwnerToken(change.OwnerToken) {
		return fmt.Errorf("%w: rollback record has no exact Remote Docker ownership token", ErrContextOwnershipLost)
	}
	var stdout bytes.Buffer
	if err := executor.Run(ctx, Invocation{
		Binary: cli,
		Args:   []string{"context", "inspect", change.Name},
		Stdout: &stdout,
		Stderr: io.Discard,
	}); err != nil {
		if change.Created && ExitCode(err) == 1 {
			return nil
		}
		return fmt.Errorf("inspect docker context %q for restore: %w", change.Name, err)
	}
	currentHost, currentDescription, currentOwner, err := managedContextState(stdout.Bytes(), change.Name)
	if err != nil {
		if errors.Is(err, ErrContextCollision) {
			return fmt.Errorf("%w: %q does not match the rollback ownership record", ErrContextOwnershipLost, change.Name)
		}
		return err
	}
	if currentHost != change.CurrentHost || currentDescription != ownedContextDescription(change.OwnerToken) || currentOwner != change.OwnerToken {
		if !change.Created && currentHost == change.PreviousHost && currentDescription == change.PreviousDescription {
			return nil
		}
		return fmt.Errorf("%w: %q changed before managed rollback", ErrContextOwnershipLost, change.Name)
	}
	if change.Created {
		removeErr := executor.Run(ctx, Invocation{
			Binary: cli,
			Args:   []string{"context", "rm", "--force", change.Name},
			Stdout: io.Discard,
			Stderr: io.Discard,
		})
		verifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		var verifyStdout bytes.Buffer
		if err := executor.Run(verifyCtx, Invocation{
			Binary: cli,
			Args:   []string{"context", "inspect", change.Name},
			Stdout: &verifyStdout,
			Stderr: io.Discard,
		}); ExitCode(err) == 1 {
			return nil
		} else if err != nil {
			return fmt.Errorf("%w: verify removal of %q: %v", ErrContextResultUnknown, change.Name, err)
		}
		host, description, owner, stateErr := managedContextState(verifyStdout.Bytes(), change.Name)
		if stateErr != nil {
			if errors.Is(stateErr, ErrContextCollision) {
				return fmt.Errorf("%w: %q appeared after managed removal", ErrContextOwnershipLost, change.Name)
			}
			return fmt.Errorf("%w: verify removal ownership for %q: %v", ErrContextResultUnknown, change.Name, stateErr)
		}
		if host == change.CurrentHost && description == ownedContextDescription(change.OwnerToken) && owner == change.OwnerToken {
			if removeErr != nil {
				return fmt.Errorf("remove newly created docker context %q: %w", change.Name, removeErr)
			}
			return fmt.Errorf("%w: %q still exists after managed removal", ErrContextResultUnknown, change.Name)
		}
		return fmt.Errorf("%w: %q appeared after managed removal", ErrContextOwnershipLost, change.Name)
	}
	if change.PreviousDescription == "" {
		return fmt.Errorf("%w: %q has no previous ownership description", ErrContextOwnershipLost, change.Name)
	}
	updateErr := updateContext(ctx, executor, cli, change.Name, change.PreviousHost, change.PreviousDescription)
	verifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	var verifyStdout bytes.Buffer
	if err := executor.Run(verifyCtx, Invocation{
		Binary: cli,
		Args:   []string{"context", "inspect", change.Name},
		Stdout: &verifyStdout,
		Stderr: io.Discard,
	}); err != nil {
		return fmt.Errorf("%w: verify restore of %q: %v", ErrContextResultUnknown, change.Name, err)
	}
	host, description, owner, err := managedContextState(verifyStdout.Bytes(), change.Name)
	if err != nil {
		if errors.Is(err, ErrContextCollision) {
			return fmt.Errorf("%w: %q changed after managed restore", ErrContextOwnershipLost, change.Name)
		}
		return fmt.Errorf("%w: verify restored ownership for %q: %v", ErrContextResultUnknown, change.Name, err)
	}
	if host == change.PreviousHost && description == change.PreviousDescription {
		return nil
	}
	if host == change.CurrentHost && description == ownedContextDescription(change.OwnerToken) && owner == change.OwnerToken {
		if updateErr != nil {
			return updateErr
		}
		return fmt.Errorf("%w: %q still has the applied ownership record after restore", ErrContextResultUnknown, change.Name)
	}
	return fmt.Errorf("%w: %q changed after managed restore", ErrContextOwnershipLost, change.Name)
}

func managedContextState(data []byte, name string) (string, string, string, error) {
	var contexts []inspectedContext
	if err := json.Unmarshal(data, &contexts); err != nil {
		return "", "", "", fmt.Errorf("decode docker context %q: %w", name, err)
	}
	if len(contexts) != 1 {
		return "", "", "", fmt.Errorf("inspect docker context %q: expected one context, got %d", name, len(contexts))
	}

	context := contexts[0]
	if context.Name != name {
		return "", "", "", fmt.Errorf("%w: %q is not owned by Remote Docker", ErrContextCollision, name)
	}
	description := context.Metadata.Description
	owner, ok := strings.CutPrefix(description, managedOwnerPrefix)
	if !ok || !validOwnerToken(owner) {
		return "", "", "", fmt.Errorf("%w: %q uses legacy or invalid ownership metadata", ErrContextCollision, name)
	}
	host := context.Endpoints.Docker.Host
	if !managedEndpoint(host) {
		return "", "", "", fmt.Errorf("%w: %q does not use a valid Remote Docker endpoint", ErrContextCollision, name)
	}
	return host, description, owner, nil
}

func ownedContextDescription(owner string) string {
	return managedOwnerPrefix + owner
}

func validOwnerToken(owner string) bool {
	if owner == "" || len(owner) > 128 || strings.TrimSpace(owner) != owner {
		return false
	}
	for _, character := range owner {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func managedEndpoint(host string) bool {
	const prefix = "ssh://remote-docker-device-"
	deviceID, ok := strings.CutPrefix(host, prefix)
	if !ok || deviceID == "" || len(deviceID) > 64 {
		return false
	}
	for _, character := range deviceID {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func updateContext(ctx context.Context, executor Executor, cli, name, host, description string) error {
	if err := executor.Run(ctx, Invocation{
		Binary: cli,
		Args:   []string{"context", "update", "--description", description, "--docker", "host=" + host, name},
		Stdout: io.Discard,
		Stderr: io.Discard,
	}); err != nil {
		return fmt.Errorf("update docker context %q: %w", name, err)
	}
	return nil
}
