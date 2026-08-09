package main

import (
	"context"
	"errors"
	"testing"
)

func TestRunMaintenanceCommandRoutesPrivateInstallerActions(t *testing.T) {
	tests := []struct {
		argument string
		wantCall string
	}{
		{argument: "--prepare-wsl", wantCall: "prepare"},
		{argument: "--delete-wsl-credential", wantCall: "delete"},
		{argument: "--shutdown", wantCall: "shutdown"},
	}
	for _, test := range tests {
		t.Run(test.argument, func(t *testing.T) {
			called := ""
			dependencies := maintenanceDependencies{
				prepareWSL: func(context.Context) error { called = "prepare"; return nil },
				deleteWSLCredential: func() error { called = "delete"; return nil },
				shutdownDesktop: func(context.Context) error { called = "shutdown"; return nil },
			}
			handled, err := runMaintenanceCommand(context.Background(), []string{test.argument}, dependencies)
			if !handled || err != nil || called != test.wantCall {
				t.Fatalf("runMaintenanceCommand() = handled %v, err %v, call %q", handled, err, called)
			}
		})
	}
}

func TestRunMaintenanceCommandRejectsFailedAction(t *testing.T) {
	want := errors.New("prepare failed")
	handled, err := runMaintenanceCommand(context.Background(), []string{"--prepare-wsl"}, maintenanceDependencies{
		prepareWSL: func(context.Context) error { return want },
	})
	if !handled || !errors.Is(err, want) {
		t.Fatalf("runMaintenanceCommand() = handled %v, err %v", handled, err)
	}
}

func TestRunMaintenanceCommandIgnoresNormalLaunch(t *testing.T) {
	handled, err := runMaintenanceCommand(context.Background(), nil, maintenanceDependencies{})
	if handled || err != nil {
		t.Fatalf("runMaintenanceCommand() = handled %v, err %v", handled, err)
	}
}
