package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/Dmitbd/remote-docker/internal/app"
	"github.com/Dmitbd/remote-docker/internal/credentials"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/provision"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if len(os.Args) == 2 && os.Args[1] == "--prepare-wsl" {
		executablePath, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "remote-docker-agent: cannot locate executable")
			os.Exit(1)
		}
		managedRuntime, err := provision.NewManagedWSLRuntime(executablePath, credentials.NewKeyringStore())
		if err != nil || managedRuntime.Prepare(ctx) != nil {
			fmt.Fprintln(os.Stderr, "remote-docker-agent: cannot prepare managed WSL runtime")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--delete-wsl-credential" {
		err := credentials.NewKeyringStore().Delete(
			provision.WindowsRuntimeCredentialOwner,
			provision.WindowsRuntimeIdentityKeyCredential,
		)
		if err != nil && !errors.Is(err, credentials.ErrNotFound) {
			fmt.Fprintln(os.Stderr, "remote-docker-agent: cannot remove managed WSL credential")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "remote-docker-agent: unsupported arguments")
		os.Exit(2)
	}
	executablePath, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "remote-docker-agent: cannot locate executable")
		os.Exit(1)
	}
	runtime, err := app.NewProductionAgentRuntime(app.ProductionAgentOptions{ExecutablePath: executablePath})
	if err != nil {
		fmt.Fprintln(os.Stderr, "remote-docker-agent: cannot initialize background runtime")
		os.Exit(1)
	}

	listener, err := localapi.Listen("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "remote-docker-agent: cannot open local control endpoint")
		os.Exit(1)
	}
	defer listener.Close()

	go func() {
		_ = runtime.Run(ctx, time.Second)
	}()
	if err := (localapi.Server{Handler: runtime.Agent()}).Serve(ctx, listener); err != nil {
		fmt.Fprintln(os.Stderr, "remote-docker-agent: local control service stopped")
		os.Exit(1)
	}
}
