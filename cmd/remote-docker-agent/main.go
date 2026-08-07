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

	listener, err := localapi.Listen("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "remote-docker-agent: cannot open local control endpoint")
		os.Exit(1)
	}
	defer listener.Close()

	agent := app.NewAgent(app.ObservationFunc(func(context.Context) app.AgentObservation {
		return app.AgentObservation{}
	}), nil, nil)
	go func() {
		_ = agent.Run(ctx, time.Second)
	}()
	if err := (localapi.Server{Handler: agent}).Serve(ctx, listener); err != nil {
		fmt.Fprintln(os.Stderr, "remote-docker-agent: local control service stopped")
		os.Exit(1)
	}
}
