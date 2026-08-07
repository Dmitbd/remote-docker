package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/Dmitbd/remote-docker/internal/app"
)

func main() {
	executablePath, err := os.Executable()
	if err != nil {
		os.Exit(1)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	os.Exit(app.RunRuntime(ctx, app.Runtime{
		ProgramName:    os.Args[0],
		ExecutablePath: executablePath,
		Env:            os.Environ(),
		Dir:            workingDirectory,
		Stdin:          os.Stdin,
		Preflight:      app.LocalAgentDockerPreflight{},
	}, os.Args[1:], os.Stdout, os.Stderr))
}
