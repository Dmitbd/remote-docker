package dockercli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// EnsureContext creates the managed context or verifies its ownership.
func EnsureContext(
	ctx context.Context,
	executor Executor,
	cli string,
	name string,
	host string,
) error {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	inspectErr := executor.Run(ctx, Invocation{
		Binary: cli,
		Args:   []string{"context", "inspect", name},
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if inspectErr == nil {
		return verifyManagedContext(stdout.Bytes(), name, host)
	}
	if ExitCode(inspectErr) != 1 {
		return fmt.Errorf("inspect docker context %q: %w", name, inspectErr)
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
		return fmt.Errorf("create docker context %q: %w", name, createErr)
	}

	return nil
}

func verifyManagedContext(data []byte, name, host string) error {
	var contexts []inspectedContext
	if err := json.Unmarshal(data, &contexts); err != nil {
		return fmt.Errorf("decode docker context %q: %w", name, err)
	}
	if len(contexts) != 1 {
		return fmt.Errorf("inspect docker context %q: expected one context, got %d", name, len(contexts))
	}

	context := contexts[0]
	if context.Name != name ||
		context.Metadata.Description != managedContextDescription ||
		context.Endpoints.Docker.Host != host {
		return fmt.Errorf(
			"%w: %q is not owned by Remote Docker or points to another endpoint",
			ErrContextCollision,
			name,
		)
	}

	return nil
}
