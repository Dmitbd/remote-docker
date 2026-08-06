package dockercli

import (
	"context"
	"errors"
)

// Port is one fixed TCP publication requested before the container exists.
type Port struct {
	HostIP        string
	HostPort      int
	ContainerPort int
}

// ReasonCode identifies a Docker mode that cannot be exposed safely.
type ReasonCode string

const (
	ReasonUnsupportedUDP ReasonCode = "unsupported_udp"
	ReasonHostNetworking ReasonCode = "host_networking"
	ReasonInvalidPort    ReasonCode = "invalid_port"
)

// Reason explains one unsupported portion of an otherwise valid invocation.
type Reason struct {
	Code   ReasonCode
	Detail string
}

// Analysis is the preflight contract consumed before the real Docker CLI runs.
type Analysis struct {
	NeedsEngine    bool
	BindSources    []string
	StaticTCPPorts []Port
	Unsupported    []Reason
}

// Analyze inspects only CLI semantics that affect synchronization and forwarding.
func Analyze(ctx context.Context, invocation Invocation, real Executor) (Analysis, error) {
	global, command, args := splitDockerCommand(invocation.Args)
	analysis := Analysis{NeedsEngine: commandNeedsEngine(command)}
	switch command {
	case "run", "create":
		mounts, ports, unsupported := analyzeRunArgs(args)
		analysis.BindSources = mounts
		analysis.StaticTCPPorts = ports
		analysis.Unsupported = unsupported
	case "compose":
		composeAnalysis, err := analyzeCompose(ctx, invocation, global, args, real)
		if err != nil {
			return Analysis{}, err
		}
		analysis.BindSources = composeAnalysis.BindSources
		analysis.Unsupported = composeAnalysis.Unsupported
	}
	return analysis, nil
}

func commandNeedsEngine(command string) bool {
	switch command {
	case "", "help", "context", "completion":
		return false
	default:
		return true
	}
}

func splitDockerCommand(args []string) (global []string, command string, commandArgs []string) {
	valueOptions := map[string]bool{
		"--config": true, "--context": true, "--host": true, "-H": true, "--log-level": true,
	}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			if index+1 < len(args) {
				return append([]string(nil), args[:index]...), args[index+1], args[index+2:]
			}
			return append([]string(nil), args[:index]...), "", nil
		}
		if len(argument) == 0 || argument[0] != '-' {
			return append([]string(nil), args[:index]...), argument, args[index+1:]
		}
		global = append(global, argument)
		if valueOptions[argument] && index+1 < len(args) {
			index++
			global = append(global, args[index])
		}
	}
	return global, "", nil
}

var errRealDockerCLIRequired = errors.New("real Docker CLI executor is required for Compose preflight")
