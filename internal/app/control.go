package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Dmitbd/remote-docker/internal/localapi"
)

const (
	ExitOK               = 0
	ExitFailure          = 1
	ExitUsage            = 2
	ExitAgentUnavailable = 3
	ExitNeedsAction      = 4
)

type ControlClient interface {
	Call(context.Context, localapi.Method, any, any) error
}

func runControlCommand(ctx context.Context, runtime Runtime, args []string, stdout, stderr io.Writer) int {
	filtered, machine := removeJSONFlag(args)
	method, params, ok := parseControlCommand(filtered)
	if !ok {
		printUsage(stderr)
		return ExitUsage
	}
	client := runtime.ControlClient
	if client == nil {
		client = localapi.Client{}
	}
	var result any
	if err := client.Call(ctx, method, params, &result); err != nil {
		fmt.Fprintf(stderr, "remote-docker: %v\n", err)
		return controlExitCode(err)
	}
	if machine {
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			fmt.Fprintln(stderr, "remote-docker: encode command result")
			return ExitFailure
		}
		return ExitOK
	}
	printHumanResult(stdout, method, result)
	return ExitOK
}

func removeJSONFlag(args []string) ([]string, bool) {
	filtered := make([]string, 0, len(args))
	machine := false
	for _, argument := range args {
		if argument == "--json" {
			machine = true
			continue
		}
		filtered = append(filtered, argument)
	}
	return filtered, machine
}

func parseControlCommand(args []string) (localapi.Method, any, bool) {
	if len(args) == 0 {
		return "", nil, false
	}
	switch args[0] {
	case "status":
		return localapi.MethodStatus, nil, len(args) == 1
	case "enable":
		return localapi.MethodEnable, nil, len(args) == 1
	case "pause":
		return localapi.MethodPause, nil, len(args) == 1
	case "search":
		if len(args) == 2 && args[1] == "start" {
			return localapi.MethodSearchStart, nil, true
		}
		if len(args) == 2 && args[1] == "stop" {
			return localapi.MethodSearchStop, nil, true
		}
	case "disconnect":
		return localapi.MethodDisconnect, localapi.DisconnectParams{}, len(args) == 1
	case "forget":
		if len(args) <= 2 {
			params := localapi.ForgetDeviceParams{}
			if len(args) == 2 {
				params.DeviceID = args[1]
			}
			return localapi.MethodForgetDevice, params, true
		}
	case "quit":
		return localapi.MethodShutdown, nil, len(args) == 1
	case "pair":
		if len(args) == 1 {
			return localapi.MethodPairStart, localapi.PairStartParams{}, true
		}
		if args[1] == "candidates" && len(args) == 2 {
			return localapi.MethodPairCandidates, nil, true
		}
		if args[1] == "start" && len(args) <= 3 {
			params := localapi.PairStartParams{}
			if len(args) == 3 {
				params.Device = args[2]
			}
			return localapi.MethodPairStart, params, true
		}
		if args[1] == "status" && len(args) == 3 {
			return localapi.MethodPairStatus, localapi.PairSessionParams{SessionID: args[2]}, true
		}
		if args[1] == "approve" && len(args) == 3 {
			return localapi.MethodPairApprove, localapi.PairSessionParams{SessionID: args[2]}, true
		}
		if args[1] == "reject" && len(args) == 3 {
			return localapi.MethodPairReject, localapi.PairSessionParams{SessionID: args[2]}, true
		}
		if args[1] == "cancel" && len(args) == 3 {
			return localapi.MethodPairCancel, localapi.PairSessionParams{SessionID: args[2]}, true
		}
	case "unpair":
		if len(args) <= 2 {
			params := localapi.ForgetDeviceParams{}
			if len(args) == 2 {
				params.DeviceID = args[1]
			}
			return localapi.MethodForgetDevice, params, true
		}
	case "workspace":
		if len(args) == 2 && args[1] == "list" {
			return localapi.MethodWorkspaceList, nil, true
		}
		if len(args) == 3 && args[1] == "add" {
			return localapi.MethodWorkspaceAdd, localapi.WorkspaceAddParams{Path: args[2]}, true
		}
		if len(args) == 3 && args[1] == "remove" {
			return localapi.MethodWorkspaceRemove, localapi.WorkspaceRemoveParams{ID: args[2]}, true
		}
	case "sync":
		if len(args) == 2 && args[1] == "status" {
			return localapi.MethodSyncStatus, nil, true
		}
	case "doctor":
		return localapi.MethodDoctor, nil, len(args) == 1
	case "recover":
		return localapi.MethodRecover, nil, len(args) == 1
	}
	return "", nil, false
}

func controlExitCode(err error) int {
	var remote *localapi.RemoteError
	if errors.As(err, &remote) {
		switch remote.Code {
		case localapi.ErrorNeedsAction:
			return ExitNeedsAction
		case localapi.ErrorUnavailable, localapi.ErrorPeerForbidden, localapi.ErrorSchemaMismatch:
			return ExitAgentUnavailable
		default:
			return ExitFailure
		}
	}
	return ExitAgentUnavailable
}

func printHumanResult(writer io.Writer, method localapi.Method, result any) {
	object, _ := result.(map[string]any)
	if method == localapi.MethodStatus {
		state, _ := object["state"].(string)
		message, _ := object["message"].(string)
		if message != "" {
			fmt.Fprintf(writer, "%s: %s\n", state, message)
		} else {
			fmt.Fprintln(writer, state)
		}
		return
	}
	if state, ok := object["state"].(string); ok {
		if message, _ := object["message"].(string); message != "" {
			fmt.Fprintf(writer, "%s: %s\n", state, message)
		} else {
			fmt.Fprintln(writer, state)
		}
		return
	}
	if items := humanItems(object); len(items) > 0 {
		for _, item := range items {
			fmt.Fprintln(writer, item)
		}
		return
	}
	fmt.Fprintf(writer, "%s completed\n", method)
}

func humanItems(object map[string]any) []string {
	for _, key := range []string{"devices", "candidates", "workspaces", "folders", "checks"} {
		values, ok := object[key].([]any)
		if !ok {
			continue
		}
		items := make([]string, 0, len(values))
		for _, value := range values {
			raw, _ := json.Marshal(value)
			items = append(items, strings.TrimSpace(string(raw)))
		}
		sort.Strings(items)
		return items
	}
	return nil
}
