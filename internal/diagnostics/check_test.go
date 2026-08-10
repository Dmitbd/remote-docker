package diagnostics

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestRunnerChecksEveryDependencyInStableOrder(t *testing.T) {
	var called []CheckName
	operations := Operations{}
	for _, name := range orderedCheckNames {
		name := name
		operations.set(name, CheckFunc(func(context.Context) error {
			called = append(called, name)
			if name == CheckDockerChannel {
				return errors.New("dockerd replied Authorization: Bearer never-show-this-token")
			}
			if name == CheckSyncChannel {
				return NewPublicError(ReasonLANBlocked)
			}
			return nil
		}))
	}

	results := Runner{Operations: operations}.Check(context.Background())
	if !reflect.DeepEqual(called, orderedCheckNames) {
		t.Fatalf("check order = %v, want %v", called, orderedCheckNames)
	}
	if len(results) != len(orderedCheckNames) {
		t.Fatalf("check count = %d, want %d", len(results), len(orderedCheckNames))
	}
	for index, result := range results {
		if result.Name != orderedCheckNames[index] {
			t.Fatalf("result[%d].Name = %q, want %q", index, result.Name, orderedCheckNames[index])
		}
	}
	if results[3].OK || results[3].Reason != string(ReasonCheckFailed) {
		t.Fatalf("Docker result = %#v, want a stable generic failure", results[3])
	}
	if results[4].OK || results[4].Reason != string(ReasonLANBlocked) {
		t.Fatalf("sync result = %#v, want explicitly safe reason", results[4])
	}
}

func TestRunnerPublishesOnlyAllowlistedTunnelFailureClasses(t *testing.T) {
	reasons := []Reason{ReasonHostUnreachable, ReasonLANBlocked, ReasonTunnelIdentityMismatch, ReasonPeerBusy, ReasonWSLUnavailable, ReasonLocalPortOccupied}
	for _, reason := range reasons {
		got := ReasonForError(NewPublicError(reason), ReasonCheckFailed)
		if got != string(reason) {
			t.Fatalf("ReasonForError(%q) = %q", reason, got)
		}
	}
	secret := "private-key signed-nonce ORDINARY_SETTING=value docker --host secret"
	if got := ReasonForError(errors.New(secret), ReasonCheckFailed); got != string(ReasonCheckFailed) {
		t.Fatalf("untrusted diagnostic error leaked as %q", got)
	}
}

func TestRunnerReportsUnavailableCheckWithStableSafeReason(t *testing.T) {
	results := (Runner{}).Check(context.Background())
	if len(results) != len(orderedCheckNames) {
		t.Fatalf("check count = %d, want %d", len(results), len(orderedCheckNames))
	}
	for index, result := range results {
		if result.Name != orderedCheckNames[index] || result.OK || result.Reason != string(ReasonCheckUnavailable) {
			t.Fatalf("result[%d] = %#v, want unavailable safe check", index, result)
		}
	}
}
