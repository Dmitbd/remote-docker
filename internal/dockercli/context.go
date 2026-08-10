package dockercli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const managedContextDescription = "Managed by Remote Docker"

// ErrContextCollision means the managed context name belongs to another endpoint.
var ErrContextCollision = errors.New("docker context name collision")

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
	Name         string
	PreviousHost string
	CurrentHost  string
	Created      bool
}

// Changed reports whether EnsureContext mutated Docker CLI state.
func (c ContextChange) Changed() bool {
	return c.Created || c.PreviousHost != c.CurrentHost
}

// EnsureContext creates the managed context or verifies its ownership.
func EnsureContext(
	ctx context.Context,
	executor Executor,
	cli string,
	name string,
	host string,
	expectedPreviousHost ...string,
) (ContextChange, error) {
	if !managedEndpoint(host) {
		return ContextChange{}, fmt.Errorf("%w: requested endpoint is not managed by Remote Docker", ErrContextCollision)
	}
	change := ContextChange{Name: name, CurrentHost: host}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	inspectErr := executor.Run(ctx, Invocation{
		Binary: cli,
		Args:   []string{"context", "inspect", name},
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if inspectErr == nil {
		previousHost, err := managedContextHost(stdout.Bytes(), name)
		if err != nil {
			return ContextChange{}, err
		}
		change.PreviousHost = previousHost
		if previousHost == host {
			return change, nil
		}
		if len(expectedPreviousHost) == 0 || expectedPreviousHost[0] == "" || previousHost != expectedPreviousHost[0] {
			return ContextChange{}, fmt.Errorf("%w: %q does not point to the exact previous managed endpoint", ErrContextCollision, name)
		}
		if err := updateContext(ctx, executor, cli, name, host); err != nil {
			return ContextChange{}, err
		}
		return change, nil
	}
	if ExitCode(inspectErr) != 1 {
		return ContextChange{}, fmt.Errorf("inspect docker context %q: %w", name, inspectErr)
	}

	createErr := executor.Run(ctx, Invocation{
		Binary: cli,
		Args: []string{
			"context", "create",
			"--description", managedContextDescription,
			"--docker", "host=" + host,
			name,
		},
		Stdout: io.Discard,
		Stderr: &stderr,
	})
	if createErr != nil {
		return ContextChange{}, fmt.Errorf("create docker context %q: %w", name, createErr)
	}

	change.Created = true
	return change, nil
}

// RestoreContext rolls back a change only while the exact managed context
// still points to the endpoint written by EnsureContext.
func RestoreContext(ctx context.Context, executor Executor, cli string, change ContextChange) error {
	if !change.Changed() {
		return nil
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
	currentHost, err := managedContextHost(stdout.Bytes(), change.Name)
	if err != nil {
		return err
	}
	if currentHost != change.CurrentHost {
		if !change.Created && currentHost == change.PreviousHost {
			return nil
		}
		return fmt.Errorf("%w: %q changed before managed rollback", ErrContextCollision, change.Name)
	}
	if change.Created {
		if err := executor.Run(ctx, Invocation{
			Binary: cli,
			Args:   []string{"context", "rm", "--force", change.Name},
			Stdout: io.Discard,
			Stderr: io.Discard,
		}); err != nil {
			return fmt.Errorf("remove newly created docker context %q: %w", change.Name, err)
		}
		return nil
	}
	return updateContext(ctx, executor, cli, change.Name, change.PreviousHost)
}

func managedContextHost(data []byte, name string) (string, error) {
	var contexts []inspectedContext
	if err := json.Unmarshal(data, &contexts); err != nil {
		return "", fmt.Errorf("decode docker context %q: %w", name, err)
	}
	if len(contexts) != 1 {
		return "", fmt.Errorf("inspect docker context %q: expected one context, got %d", name, len(contexts))
	}

	context := contexts[0]
	if context.Name != name ||
		context.Metadata.Description != managedContextDescription {
		return "", fmt.Errorf("%w: %q is not owned by Remote Docker", ErrContextCollision, name)
	}
	host := context.Endpoints.Docker.Host
	if !managedEndpoint(host) {
		return "", fmt.Errorf("%w: %q does not use a valid Remote Docker endpoint", ErrContextCollision, name)
	}
	return host, nil
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

func updateContext(ctx context.Context, executor Executor, cli, name, host string) error {
	if err := executor.Run(ctx, Invocation{
		Binary: cli,
		Args:   []string{"context", "update", "--docker", "host=" + host, name},
		Stdout: io.Discard,
		Stderr: io.Discard,
	}); err != nil {
		return fmt.Errorf("update docker context %q: %w", name, err)
	}
	return nil
}
