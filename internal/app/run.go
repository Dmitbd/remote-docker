package app

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

const version = "dev"

// Run executes the remote-docker command and returns a process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	return RunRuntime(context.Background(), Runtime{ProgramName: "remote-docker"}, args, stdout, stderr)
}

// RunRuntime executes the command with explicit process dependencies.
func RunRuntime(ctx context.Context, runtime Runtime, args []string, stdout, stderr io.Writer) int {
	programName := strings.TrimSuffix(filepath.Base(runtime.ProgramName), ".exe")
	if programName == "docker" {
		return runDocker(ctx, runtime, args, stdout, stderr)
	}
	if len(args) > 0 && args[0] == "docker" {
		return runDocker(ctx, runtime, args[1:], stdout, stderr)
	}

	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "remote-docker %s\n", version)
		return 0
	case "status", "pair", "unpair", "workspace", "sync", "doctor", "recover":
		return runControlCommand(ctx, runtime, args, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: remote-docker <command>")
	fmt.Fprintln(w, "commands: status, pair, unpair, workspace, sync status, doctor, recover, docker")
}
