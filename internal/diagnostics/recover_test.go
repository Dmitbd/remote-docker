package diagnostics

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestRecoveryObservesFreshReadinessAfterEveryActionAndContinuesUntilConfirmed(t *testing.T) {
	var called []RecoveryStep
	observations := 0
	operations := RecoveryOperations{
		Reconnect:          recordRecovery(&called, RecoveryReconnect, NewPublicError(ReasonRemoteConnectionNotReady)),
		RestartUserProcess: recordRecovery(&called, RecoveryRestartUserProcess, errors.New("ORDINARY_SETTING=secret with spaces")),
		StartWSLDistro:     recordRecovery(&called, RecoveryStartWSLDistro, nil),
		RestartSystemdUnit: recordRecovery(&called, RecoveryRestartSystemdUnit, nil),
	}
	ready := ReadinessFunc(func(context.Context) (bool, error) {
		observations++
		return observations == 4, nil
	})

	result, err := (Recoverer{Operations: operations, Readiness: ready}).Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	want := []RecoveryStep{RecoveryReconnect, RecoveryRestartUserProcess, RecoveryStartWSLDistro, RecoveryRestartSystemdUnit}
	if !reflect.DeepEqual(called, want) {
		t.Fatalf("recovery order = %v, want %v", called, want)
	}
	if observations != len(want) {
		t.Fatalf("readiness observations = %d, want one fresh observation after every action", observations)
	}
	if result.Step != RecoveryRestartSystemdUnit || len(result.Attempts) != len(want) || !result.Attempts[3].OK {
		t.Fatalf("result = %#v, want confirmed systemd recovery", result)
	}
	if result.Attempts[0].Reason != string(ReasonRemoteConnectionNotReady) {
		t.Fatalf("reconnect attempt = %#v, want explicitly safe reason", result.Attempts[0])
	}
	if result.Attempts[1].Reason != string(ReasonRecoveryOperationFailed) {
		t.Fatalf("user-process attempt = %#v, want generic safe reason", result.Attempts[1])
	}
	if result.Attempts[2].Reason != string(ReasonRecoveryNotConfirmed) {
		t.Fatalf("WSL attempt = %#v, want readiness-gating reason", result.Attempts[2])
	}
}

func TestRecoveryDoesNotStopOnNilActionWithoutFreshReadiness(t *testing.T) {
	var called []RecoveryStep
	observations := 0
	result, err := (Recoverer{
		Operations: RecoveryOperations{
			Reconnect:          recordRecovery(&called, RecoveryReconnect, nil),
			RestartUserProcess: recordRecovery(&called, RecoveryRestartUserProcess, nil),
		},
		Readiness: ReadinessFunc(func(context.Context) (bool, error) {
			observations++
			return observations == 2, nil
		}),
	}).Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if !reflect.DeepEqual(called, []RecoveryStep{RecoveryReconnect, RecoveryRestartUserProcess}) || observations != 2 {
		t.Fatalf("calls=%v observations=%d, want two gated actions", called, observations)
	}
	if result.Attempts[0].OK || result.Attempts[0].Reason != string(ReasonRecoveryNotConfirmed) || !result.Attempts[1].OK {
		t.Fatalf("attempts = %#v, want only freshly confirmed second action OK", result.Attempts)
	}
}

func TestRecoveryStopsWhenFreshObservationConfirmsRecoveryDespiteActionError(t *testing.T) {
	var called []RecoveryStep
	result, err := (Recoverer{
		Operations: RecoveryOperations{
			Reconnect:          recordRecovery(&called, RecoveryReconnect, errors.New("internal reconnect detail token=secret")),
			RestartUserProcess: recordRecovery(&called, RecoveryRestartUserProcess, nil),
		},
		Readiness: ReadinessFunc(func(context.Context) (bool, error) { return true, nil }),
	}).Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if !reflect.DeepEqual(called, []RecoveryStep{RecoveryReconnect}) || result.Step != RecoveryReconnect || !result.Attempts[0].OK {
		t.Fatalf("result=%#v calls=%v, want freshly confirmed reconnect only", result, called)
	}
}

func TestRecoveryReturnsSafePublicAttemptsWhenReadinessNeverRecovers(t *testing.T) {
	observations := 0
	result, err := (Recoverer{
		Operations: RecoveryOperations{
			Reconnect: recordRecovery(nil, RecoveryReconnect, errors.New("pairing_token=never-show-this-token")),
		},
		Readiness: ReadinessFunc(func(context.Context) (bool, error) {
			observations++
			return false, errors.New("Authorization: Bearer never-show-this-token")
		}),
	}).Recover(context.Background())
	if !errors.Is(err, ErrRecoveryFailed) {
		t.Fatalf("Recover() error = %v, want %v", err, ErrRecoveryFailed)
	}
	if observations != 1 {
		t.Fatalf("observations = %d, want one after the only configured action", observations)
	}
	if result.Step != "" || len(result.Attempts) != len(orderedRecoverySteps) {
		t.Fatalf("result = %#v, want complete safe attempt list", result)
	}
	if result.Attempts[0].Reason != string(ReasonRecoveryOperationFailed) {
		t.Fatalf("reconnect attempt = %#v, want generic safe reason", result.Attempts[0])
	}
	for _, attempt := range result.Attempts {
		if attempt.Reason == "pairing_token=never-show-this-token" || attempt.Reason == "Authorization: Bearer never-show-this-token" {
			t.Fatalf("attempt leaked secret: %#v", attempt)
		}
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
