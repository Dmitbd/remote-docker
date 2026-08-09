package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	gouruntime "runtime"
	"time"

	"github.com/Dmitbd/remote-docker/internal/app"
	"github.com/Dmitbd/remote-docker/internal/credentials"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/lifecycle"
	"github.com/Dmitbd/remote-docker/internal/provision"
)

func main() {
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt)
	ctx, stop := context.WithCancel(ctx)
	defer stopSignals()
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

	role := lifecycle.RoleMacClient
	if gouruntime.GOOS == "windows" {
		role = lifecycle.RoleWindowsHost
	}
	if err := runtime.Start(ctx, role); err != nil {
		fmt.Fprintln(os.Stderr, "remote-docker-agent: background runtime could not start")
		os.Exit(1)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- (localapi.Server{Handler: runtime.Agent()}).Serve(ctx, listener) }()
	select {
	case runtimeErr := <-runtime.Done():
		stop()
		_ = listener.Close()
		<-serveDone
		if runtimeErr != nil && !errors.Is(runtimeErr, context.Canceled) {
			fmt.Fprintln(os.Stderr, "remote-docker-agent: background runtime stopped")
			os.Exit(1)
		}
	case serveErr := <-serveDone:
		stop()
		stopCtx, cancelStop := context.WithTimeout(context.Background(), 30*time.Second)
		_ = runtime.Stop(stopCtx, lifecycle.StopQuit)
		cancelStop()
		if serveErr != nil {
			fmt.Fprintln(os.Stderr, "remote-docker-agent: local control service stopped")
			os.Exit(1)
		}
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "remote-docker-agent: local control service stopped")
		os.Exit(1)
	}
}
