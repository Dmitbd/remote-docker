package desktopui

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/localapi"
)

func TestOperationGuardRejectsDuplicatesAndConflictsAndAlwaysReleases(t *testing.T) {
	guard := NewOperationGuard()
	release, err := guard.Begin(OperationConnect)
	if err != nil {
		t.Fatalf("Begin(connect) error = %v", err)
	}
	if _, err := guard.Begin(OperationConnect); !errors.Is(err, ErrOperationPending) {
		t.Fatalf("duplicate error = %v, want ErrOperationPending", err)
	}
	if _, err := guard.Begin(OperationPause); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("conflict error = %v, want ErrOperationConflict", err)
	}
	release()
	if _, err := guard.Begin(OperationPause); err != nil {
		t.Fatalf("guard remained locked after release: %v", err)
	}
}

func TestBackendSnapshotUsesOwnerOnlyLocalAPIAndPreservesRevision(t *testing.T) {
	handler := &desktopUIHandler{responses: map[localapi.Method]any{
		localapi.MethodStatus: localapi.StatusResult{
			Revision: 42, Role: "mac_client", State: "client_ready", LocalName: "Mac",
		},
		localapi.MethodPairCandidates: localapi.PairCandidatesResult{Candidates: []localapi.PairingCandidate{
			{ID: "windows", Name: "Windows", Available: true},
		}},
		localapi.MethodWorkspaceList:  localapi.WorkspaceListResult{},
		localapi.MethodSyncStatus:     localapi.SyncStatusResult{},
		localapi.MethodResourceStatus: localapi.ResourceStatusResult{},
		localapi.MethodDoctor:         localapi.DoctorResult{},
	}}
	backend := NewBackend(localClientForTest(handler), "darwin")
	state, err := backend.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if state.Revision != 42 || state.Platform != "darwin" || len(state.Devices) != 1 {
		t.Fatalf("Snapshot() = %#v", state)
	}
}

func TestBackendPerformPublishesPendingStateAndClearsItAfterFailure(t *testing.T) {
	started := make(chan struct{})
	unblock := make(chan struct{})
	handler := &desktopUIHandler{
		responses: map[localapi.Method]any{
			localapi.MethodStatus: localapi.StatusResult{Revision: 3, Role: "mac_client", State: "searching"},
			localapi.MethodPairCandidates: localapi.PairCandidatesResult{Candidates: []localapi.PairingCandidate{
				{ID: "windows", Name: "Windows", Available: true},
			}},
		},
		call: func(method localapi.Method) error {
			if method == localapi.MethodPairStart {
				close(started)
				<-unblock
				return &localapi.PublicError{Code: localapi.ErrorUnavailable, Message: "Windows недоступен"}
			}
			return nil
		},
	}
	backend := NewBackend(localClientForTest(handler), "darwin")
	done := make(chan error, 1)
	go func() {
		_, err := backend.Perform(context.Background(), OperationConnect, "windows")
		done <- err
	}()
	<-started
	state, err := backend.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("pending Snapshot() error = %v", err)
	}
	pending := operationByID(t, state.Devices[0].Operations, OperationConnect)
	if !pending.Pending || pending.Enabled || pending.PendingLabel != "Подключаемся…" {
		t.Fatalf("pending connect = %#v", pending)
	}
	if _, err := backend.Perform(context.Background(), OperationPause, ""); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("conflicting Perform() error = %v", err)
	}
	close(unblock)
	if err := <-done; err == nil {
		t.Fatal("Perform(connect) succeeded, want failure")
	}
	state, err = backend.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("terminal Snapshot() error = %v", err)
	}
	if operationByID(t, state.Devices[0].Operations, OperationConnect).Pending {
		t.Fatal("failed operation retained pending state")
	}
}

func operationByID(t *testing.T, operations []Operation, id string) Operation {
	t.Helper()
	for _, operation := range operations {
		if operation.ID == id {
			return operation
		}
	}
	t.Fatalf("operation %q not found in %#v", id, operations)
	return Operation{}
}

type desktopUIHandler struct {
	responses map[localapi.Method]any
	call      func(localapi.Method) error
}

func (h *desktopUIHandler) Handle(_ context.Context, method localapi.Method, _ json.RawMessage) (any, error) {
	if h.call != nil {
		if err := h.call(method); err != nil {
			return nil, err
		}
	}
	return h.responses[method], nil
}

func localClientForTest(handler localapi.Handler) localapi.Client {
	return localapi.Client{Dial: func(ctx context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			_ = (localapi.Server{
				Handler:       handler,
				AuthorizePeer: func(net.Conn) error { return nil },
			}).ServeConn(ctx, server)
		}()
		return client, nil
	}}
}
