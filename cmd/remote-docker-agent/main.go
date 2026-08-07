package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/Dmitbd/remote-docker/internal/app"
	"github.com/Dmitbd/remote-docker/internal/localapi"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
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
