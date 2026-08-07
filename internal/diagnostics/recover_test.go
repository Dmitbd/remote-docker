package diagnostics

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestRecoveryUsesNonDestructiveTypedLadderInOrder(t *testing.T) {
	var called []RecoveryStep
	operations := RecoveryOperations{
		Reconnect:          recordRecovery(&called, RecoveryReconnect, errors.New("network unavailable")),
		RestartUserProcess: recordRecovery(&called, RecoveryRestartUserProcess, errors.New("sync token=never-show-this-token")),
		StartWSLDistro:     recordRecovery(&called, RecoveryStartWSLDistro, errors.New("wsl is stopped")),
		RestartSystemdUnit: recordRecovery(&called, RecoveryRestartSystemdUnit, nil),
	}

	result, err := Recoverer{Operations: operations}.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	want := []RecoveryStep{RecoveryReconnect, RecoveryRestartUserProcess, RecoveryStartWSLDistro, RecoveryRestartSystemdUnit}
	if !reflect.DeepEqual(called, want) {
		t.Fatalf("recovery order = %v, want %v", called, want)
	}
	if result.Step != RecoveryRestartSystemdUnit || len(result.Attempts) != len(want) {
		t.Fatalf("result = %#v, want completed systemd recovery", result)
	}
	if result.Attempts[1].Reason == "sync token=never-show-this-token" {
		t.Fatalf("recovery reason leaked a secret: %#v", result.Attempts[1])
	}
}

func TestRecoveryStopsAtFirstSuccessfulSafeOperation(t *testing.T) {
	var called []RecoveryStep
	result, err := (Recoverer{Operations: RecoveryOperations{
		Reconnect:          recordRecovery(&called, RecoveryReconnect, nil),
		RestartUserProcess: recordRecovery(&called, RecoveryRestartUserProcess, nil),
	}}).Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if !reflect.DeepEqual(called, []RecoveryStep{RecoveryReconnect}) || result.Step != RecoveryReconnect {
		t.Fatalf("result=%#v calls=%v, want reconnect only", result, called)
	}
}

func TestRecoveryReturnsStablePublicErrorWhenNoOperationSucceeds(t *testing.T) {
	result, err := (Recoverer{Operations: RecoveryOperations{
		Reconnect: recordRecovery(nil, RecoveryReconnect, errors.New("pairing_token=never-show-this-token")),
	}}).Recover(context.Background())
	if !errors.Is(err, ErrRecoveryFailed) {
		t.Fatalf("Recover() error = %v, want %v", err, ErrRecoveryFailed)
	}
	if result.Step != "" || len(result.Attempts) != len(orderedRecoverySteps) || result.Attempts[0].Reason == "pairing_token=never-show-this-token" {
		t.Fatalf("result = %#v, want safe failed attempts", result)
	}
}

func recordRecovery(called *[]RecoveryStep, step RecoveryStep, err error) RecoveryFunc {
	return func(context.Context) error {
		if called != nil {
			*called = append(*called, step)
		}
		return err
	}
}
