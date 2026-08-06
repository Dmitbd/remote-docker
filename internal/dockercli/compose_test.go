package dockercli

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestAnalyzeComposeMutationsUsesRealCLIConfig(t *testing.T) {
	executor := &composeExecutor{output: map[string]any{
		"services": map[string]any{
			"web": map[string]any{
				"volumes": []any{
					map[string]any{"type": "bind", "source": "/Users/demo/project", "target": "/app"},
					map[string]any{"type": "volume", "source": "cache", "target": "/cache"},
				},
			},
			"worker": map[string]any{
				"volumes": []any{map[string]any{"type": "bind", "source": "/Users/demo/shared", "target": "/shared"}},
			},
		},
	}}
	invocation := Invocation{
		Binary: "docker-real",
		Args: []string{
			"--context", "remote-docker", "compose",
			"-f", "compose.yml",
			"--project-directory", "/Users/demo/project",
			"--env-file=.env.dev",
			"--profile", "workers",
			"--project-name", "sample",
			"up", "-d",
		},
		Dir: "/Users/demo/project",
		Env: []string{"HOME=/Users/demo"},
	}
	analysis, err := Analyze(context.Background(), invocation, executor)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/Users/demo/project", "/Users/demo/shared"}; !reflect.DeepEqual(analysis.BindSources, want) {
		t.Fatalf("BindSources = %#v, want %#v", analysis.BindSources, want)
	}
	wantArgs := []string{
		"--context", "remote-docker", "compose",
		"-f", "compose.yml",
		"--project-directory", "/Users/demo/project",
		"--env-file=.env.dev",
		"--profile", "workers",
		"--project-name", "sample",
		"config", "--format", "json",
	}
	if !reflect.DeepEqual(executor.invocation.Args, wantArgs) || executor.invocation.Binary != "docker-real" {
		t.Fatalf("config invocation = %#v, want args %#v", executor.invocation, wantArgs)
	}
	if executor.invocation.Dir != invocation.Dir || !reflect.DeepEqual(executor.invocation.Env, invocation.Env) {
		t.Fatalf("config process context not preserved: %#v", executor.invocation)
	}
}

func TestAnalyzeComposeReadOnlyCommandsSkipSyncPreflight(t *testing.T) {
	for _, command := range []string{"down", "logs", "ps", "exec", "pull"} {
		t.Run(command, func(t *testing.T) {
			executor := &composeExecutor{err: errors.New("must not run")}
			analysis, err := Analyze(context.Background(), Invocation{
				Binary: "docker-real",
				Args:   []string{"--context", "remote-docker", "compose", "-f", "compose.yml", command},
			}, executor)
			if err != nil {
				t.Fatal(err)
			}
			if executor.calls != 0 || len(analysis.BindSources) != 0 {
				t.Fatalf("command %s triggered sync preflight", command)
			}
		})
	}
}

type composeExecutor struct {
	output     any
	err        error
	calls      int
	invocation Invocation
}

func (e *composeExecutor) Run(_ context.Context, invocation Invocation) error {
	e.calls++
	e.invocation = invocation
	if e.err != nil {
		return e.err
	}
	if invocation.Stdout == nil {
		return errors.New("missing config stdout")
	}
	encoded, err := json.Marshal(e.output)
	if err != nil {
		return err
	}
	_, err = invocation.Stdout.Write(encoded)
	return err
}
