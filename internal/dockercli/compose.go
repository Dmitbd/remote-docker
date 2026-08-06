package dockercli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

var composeMutatingCommands = map[string]bool{
	"up": true, "run": true, "create": true, "start": true,
}

var composeValueOptions = map[string]bool{
	"-f": true, "--file": true,
	"--project-directory": true,
	"--env-file":          true,
	"--profile":           true,
	"--project-name":      true, "-p": true,
}

func analyzeCompose(ctx context.Context, invocation Invocation, dockerGlobal, args []string, real Executor) (Analysis, error) {
	composeGlobal, command := splitComposeCommand(args)
	if !composeMutatingCommands[command] {
		return Analysis{}, nil
	}
	if real == nil {
		return Analysis{}, errRealDockerCLIRequired
	}

	configArgs := make([]string, 0, len(dockerGlobal)+len(composeGlobal)+4)
	configArgs = append(configArgs, dockerGlobal...)
	configArgs = append(configArgs, "compose")
	configArgs = append(configArgs, composeGlobal...)
	configArgs = append(configArgs, "config", "--format", "json")
	var stdout bytes.Buffer
	err := real.Run(ctx, Invocation{
		Binary: invocation.Binary,
		Args:   configArgs,
		Env:    invocation.Env,
		Dir:    invocation.Dir,
		Stdin:  invocation.Stdin,
		Stdout: &stdout,
		Stderr: io.Discard,
	})
	if err != nil {
		return Analysis{}, fmt.Errorf("resolve Docker Compose configuration: %w", err)
	}

	var config struct {
		Services map[string]struct {
			Volumes []struct {
				Type   string `json:"type"`
				Source string `json:"source"`
			} `json:"volumes"`
		} `json:"services"`
	}
	decoder := json.NewDecoder(io.LimitReader(&stdout, 16<<20))
	if err := decoder.Decode(&config); err != nil {
		return Analysis{}, fmt.Errorf("decode Docker Compose config JSON: %w", err)
	}

	serviceNames := make([]string, 0, len(config.Services))
	for name := range config.Services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)
	seen := make(map[string]struct{})
	var sources []string
	for _, name := range serviceNames {
		for _, volume := range config.Services[name].Volumes {
			if !strings.EqualFold(volume.Type, "bind") || volume.Source == "" {
				continue
			}
			if _, exists := seen[volume.Source]; exists {
				continue
			}
			seen[volume.Source] = struct{}{}
			sources = append(sources, volume.Source)
		}
	}
	return Analysis{BindSources: sources}, nil
}

func splitComposeCommand(args []string) (global []string, command string) {
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			if index+1 < len(args) {
				return global, args[index+1]
			}
			return global, ""
		}
		if argument == "" || argument[0] != '-' {
			return global, argument
		}
		name, _, inline := splitOption(argument)
		global = append(global, argument)
		if composeValueOptions[name] && !inline && index+1 < len(args) {
			index++
			global = append(global, args[index])
		}
	}
	return global, ""
}
