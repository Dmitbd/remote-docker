package desktopui

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestBackendCanonicalizesMacWorkspaceBeforeRegistration(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "project-alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	var received localapi.WorkspaceAddParams
	handler := &desktopUIHandler{
		responses: map[localapi.Method]any{
			localapi.MethodStatus:         localapi.StatusResult{Role: "mac_client", State: "client_ready"},
			localapi.MethodWorkspaceList:  localapi.WorkspaceListResult{},
			localapi.MethodSyncStatus:     localapi.SyncStatusResult{},
			localapi.MethodResourceStatus: localapi.ResourceStatusResult{},
			localapi.MethodDoctor:         localapi.DoctorResult{},
		},
		rawCall: func(method localapi.Method, raw json.RawMessage) error {
			if method == localapi.MethodWorkspaceAdd {
				return json.Unmarshal(raw, &received)
			}
			return nil
		},
	}
	backend := NewBackend(localClientForTest(handler), "darwin")
	if _, err := backend.Perform(context.Background(), OperationAddProject, alias); err != nil {
		t.Fatalf("Perform(add-project) error = %v", err)
	}
	want, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	if received.Path != want {
		t.Fatalf("workspace path = %q, want canonical %q", received.Path, want)
	}
}

func TestBackendRejectsWorkspaceSelectionOutsideMacAndManualPublicAddress(t *testing.T) {
	backend := NewBackend(localapi.Client{}, "windows")
	if _, _, err := backend.resolve(context.Background(), OperationAddProject, `C:\project`); err == nil {
		t.Fatal("Windows UI accepted a workspace selection")
	}
	backend.Platform = "darwin"
	if _, _, err := backend.resolve(context.Background(), OperationManualAddress, "203.0.113.8"); err == nil {
		t.Fatal("manual pairing accepted a public address")
	}
}

func TestBackendPairingCancellationUsesDisplayedSessionWithoutStatusRead(t *testing.T) {
	calls := 0
	handler := &desktopUIHandler{
		responses: map[localapi.Method]any{
			localapi.MethodStatus: localapi.StatusResult{Pairing: &localapi.PairingStatusResult{SessionID: "newer-session"}},
		},
		call: func(localapi.Method) error {
			calls++
			return nil
		},
	}
	backend := NewBackend(localClientForTest(handler), "darwin")
	method, rawParams, err := backend.resolve(context.Background(), OperationCancelPair, "displayed-session")
	if err != nil {
		t.Fatalf("resolve(cancel-pair) error = %v", err)
	}
	params, ok := rawParams.(localapi.PairSessionParams)
	if method != localapi.MethodConnectionCancel || !ok || params.SessionID != "displayed-session" {
		t.Fatalf("resolved cancel = method %q params %#v", method, rawParams)
	}
	if calls != 0 {
		t.Fatalf("resolve(cancel-pair) made %d status calls, want none", calls)
	}
}

func TestBackendStopConnectionCallsLocalAPIWithBoundedContext(t *testing.T) {
	called := false
	deadlineSet := false
	handler := &desktopUIHandler{
		responses: map[localapi.Method]any{
			localapi.MethodStatus:         localapi.StatusResult{Role: "windows_host", State: "connecting"},
			localapi.MethodWorkspaceList:  localapi.WorkspaceListResult{},
			localapi.MethodSyncStatus:     localapi.SyncStatusResult{},
			localapi.MethodResourceStatus: localapi.ResourceStatusResult{},
			localapi.MethodDoctor:         localapi.DoctorResult{},
		},
		callWithContext: func(ctx context.Context, method localapi.Method) error {
			if method == localapi.MethodConnectionCancel {
				called = true
				_, deadlineSet = ctx.Deadline()
			}
			return nil
		},
	}

	if _, err := NewBackend(localClientForTest(handler), "windows").Perform(context.Background(), OperationStopConnection, ""); err != nil {
		t.Fatalf("Perform(stop-connection) error = %v", err)
	}
	if !called || !deadlineSet {
		t.Fatalf("ConnectionCancel called = %t, bounded context = %t", called, deadlineSet)
	}
	if timeout := operationTimeout(OperationStopConnection); timeout != 90*time.Second {
		t.Fatalf("stop connection timeout = %s, want 90s", timeout)
	}
}

func TestEveryMutatingDesktopOperationHasAnExplicitTimeout(t *testing.T) {
	for _, id := range []string{
		OperationConnect, OperationConnectTrusted, OperationManualAddress,
		OperationDisconnect, OperationForgetDevice,
		OperationApprovePair, OperationRejectPair, OperationCancelPair,
		OperationStopConnection,
		OperationPause, OperationAddProject, OperationQuit,
	} {
		if timeout := operationTimeout(id); timeout <= 0 || timeout > 2*time.Minute {
			t.Fatalf("operationTimeout(%q) = %s, want bounded timeout", id, timeout)
		}
	}
}

func TestBackendSnapshotUsesOwnerOnlyLocalAPIAndPreservesRevision(t *testing.T) {
	handler := &desktopUIHandler{responses: map[localapi.Method]any{
		localapi.MethodStatus: localapi.StatusResult{
			Revision: 42, Role: "mac_client", State: "searching", LocalName: "Mac",
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

func TestBackendSnapshotRunsDiscoveryOnlyWhileSearching(t *testing.T) {
	for _, test := range []struct {
		name           string
		state          string
		wantDiscovery  int
		wantDeviceKind string
	}{
		{name: "active search", state: "searching", wantDiscovery: 1, wantDeviceKind: "saved"},
		{name: "ready without search", state: "client_ready", wantDeviceKind: "saved"},
		{name: "paused", state: "paused", wantDeviceKind: "saved"},
	} {
		t.Run(test.name, func(t *testing.T) {
			discoveryCalls := 0
			handler := &desktopUIHandler{
				responses: map[localapi.Method]any{
					localapi.MethodStatus: localapi.StatusResult{
						Revision: 7,
						Role:     "mac_client",
						State:    test.state,
						Peer:     &localapi.LifecyclePeer{ID: "saved-windows", Name: "Saved Windows"},
					},
					localapi.MethodPairCandidates: localapi.PairCandidatesResult{Candidates: []localapi.PairingCandidate{
						{ID: "saved-windows", Name: "Saved Windows", Trusted: true, Available: true},
					}},
					localapi.MethodWorkspaceList:  localapi.WorkspaceListResult{},
					localapi.MethodSyncStatus:     localapi.SyncStatusResult{},
					localapi.MethodResourceStatus: localapi.ResourceStatusResult{},
					localapi.MethodDoctor:         localapi.DoctorResult{},
				},
				call: func(method localapi.Method) error {
					if method == localapi.MethodPairCandidates {
						discoveryCalls++
					}
					return nil
				},
			}

			state, err := NewBackend(localClientForTest(handler), "darwin").Snapshot(context.Background())
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			if discoveryCalls != test.wantDiscovery {
				t.Fatalf("PairCandidates calls = %d, want %d in state %q", discoveryCalls, test.wantDiscovery, test.state)
			}
			if len(state.Devices) != 1 || state.Devices[0].ID != "saved-windows" || state.Devices[0].Kind != test.wantDeviceKind {
				t.Fatalf("saved device = %#v", state.Devices)
			}
		})
	}
}

func TestBackendPerformPublishesPendingStateAndClearsItAfterFailure(t *testing.T) {
	started := make(chan struct{})
	unblock := make(chan struct{})
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	}()
	handler := &desktopUIHandler{
		responses: map[localapi.Method]any{
			localapi.MethodStatus: localapi.StatusResult{
				Revision: 3, Role: "mac_client", State: "pairing",
				Pairing: &localapi.PairingStatusResult{SessionID: "session-1"},
			},
		},
		call: func(method localapi.Method) error {
			if method == localapi.MethodConnectionCancel || method == localapi.MethodPairCancel {
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
		_, err := backend.Perform(context.Background(), OperationCancelPair, "session-1")
		done <- err
	}()
	<-started
	state, err := backend.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("pending Snapshot() error = %v", err)
	}
	pending := operationByID(t, state.Operations, OperationCancelPair)
	if !pending.Pending || pending.Enabled || pending.PendingLabel != "Отменяем…" {
		t.Fatalf("pending connection cancellation = %#v", pending)
	}
	if _, err := backend.Perform(context.Background(), OperationPause, ""); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("conflicting Perform() error = %v", err)
	}
	close(unblock)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "отменить подключение") {
		t.Fatalf("Perform(cancel-pair) error = %v, want public cancellation error", err)
	}
	state, err = backend.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("terminal Snapshot() error = %v", err)
	}
	if operationByID(t, state.Operations, OperationCancelPair).Pending {
		t.Fatal("failed cancellation retained pending state")
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
	responses       map[localapi.Method]any
	call            func(localapi.Method) error
	callWithContext func(context.Context, localapi.Method) error
	rawCall         func(localapi.Method, json.RawMessage) error
}

func (h *desktopUIHandler) Handle(ctx context.Context, method localapi.Method, raw json.RawMessage) (any, error) {
	if h.callWithContext != nil {
		if err := h.callWithContext(ctx, method); err != nil {
			return nil, err
		}
	}
	if h.call != nil {
		if err := h.call(method); err != nil {
			return nil, err
		}
	}
	if h.rawCall != nil {
		if err := h.rawCall(method, raw); err != nil {
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
