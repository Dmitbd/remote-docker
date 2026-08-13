package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/credentials"
	"github.com/Dmitbd/remote-docker/internal/discovery"
	"github.com/Dmitbd/remote-docker/internal/dockercli"
	"github.com/Dmitbd/remote-docker/internal/lifecycle"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/pairing"
	"github.com/Dmitbd/remote-docker/internal/portrelay"
	"github.com/Dmitbd/remote-docker/internal/sshtransport"
	"github.com/Dmitbd/remote-docker/internal/syncer"
	"github.com/Dmitbd/remote-docker/internal/tunnel"
	"github.com/Dmitbd/remote-docker/internal/windowsbridge"
	"golang.org/x/crypto/ssh"
)

func TestProductionAgentRuntimeServesPersistedStateOverLocalSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket integration is covered on Unix builders")
	}
	root, err := os.MkdirTemp("/private/tmp", "rd-agent-e2e-")
	if err != nil {
		t.Fatalf("create short test runtime root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("protect test runtime root: %v", err)
	}
	configPath := filepath.Join(root, "config.json")
	store := config.Store{Path: configPath}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		Devices: map[string]config.Device{
			"pc-1": {Name: "Dev PC", Address: "192.168.1.20", SSHPort: 2222},
		},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	runtimeAgent, err := NewProductionAgentRuntime(ProductionAgentOptions{
		ConfigPath:     configPath,
		ExecutablePath: filepath.Join(root, "bin", "remote-docker-agent"),
	})
	if err != nil {
		t.Fatalf("NewProductionAgentRuntime() error = %v", err)
	}
	controller, ok := runtimeAgent.agent.controller.(*productionAgentController)
	preparer, prepared := controller.dockerPreparer.(*productionDockerPreparer)
	if !ok || !prepared || preparer.sync == nil {
		t.Fatal("production runtime did not wire the Docker preflight preparer")
	}
	readiness, ok := preparer.sync.(productionSyncReadiness)
	if !ok || readiness.httpClient == nil || readiness.httpClient.Timeout != defaultPreflightTimeout {
		t.Fatalf("Syncthing preflight client timeout = %#v, want %s", readiness.httpClient, defaultPreflightTimeout)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runDone := make(chan error, 1)
	go func() { runDone <- runtimeAgent.Run(ctx, 5*time.Millisecond) }()

	endpoint := filepath.Join(root, "agent.sock")
	listener, err := localapi.Listen(endpoint)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- (localapi.Server{Handler: runtimeAgent.Agent()}).Serve(ctx, listener) }()

	client := localapi.Client{Endpoint: endpoint}
	var devices localapi.ListDevicesResult
	if err := client.Call(ctx, localapi.MethodListDevices, nil, &devices); err != nil {
		t.Fatalf("ListDevices error = %v", err)
	}
	if len(devices.Devices) != 1 || devices.Devices[0].ID != "pc-1" || devices.Devices[0].Name != "Dev PC" {
		t.Fatalf("devices = %#v", devices.Devices)
	}

	workspacePath := filepath.Join(root, "sample")
	if err := os.Mkdir(workspacePath, 0o700); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	var added localapi.Workspace
	if err := client.Call(ctx, localapi.MethodWorkspaceAdd, localapi.WorkspaceAddParams{Path: workspacePath}, &added); err != nil {
		t.Fatalf("WorkspaceAdd error = %v", err)
	}
	if added.ID == "" || added.Path != workspacePath {
		t.Fatalf("added workspace = %#v", added)
	}
	var workspaces localapi.WorkspaceListResult
	if err := client.Call(ctx, localapi.MethodWorkspaceList, nil, &workspaces); err != nil {
		t.Fatalf("WorkspaceList error = %v", err)
	}
	if len(workspaces.Workspaces) != 1 || workspaces.Workspaces[0] != added {
		t.Fatalf("workspaces = %#v, want %#v", workspaces.Workspaces, added)
	}
	persisted, err := store.Load()
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if persisted.Workspaces[added.ID].Path != workspacePath {
		t.Fatalf("persisted workspaces = %#v", persisted.Workspaces)
	}

	var status localapi.StatusResult
	if err := client.Call(ctx, localapi.MethodStatus, nil, &status); err != nil {
		t.Fatalf("Status error = %v", err)
	}
	if status.State != string(AgentUnpaired) {
		t.Fatalf("status = %#v, want Unpaired", status)
	}
	var recovered localapi.RecoverResult
	err = client.Call(ctx, localapi.MethodRecover, nil, &recovered)
	if err != nil {
		t.Fatalf("Recover error = %v", err)
	}
	if recovered.State != string(AgentUnpaired) || recovered.Message != "pair a device to continue" || len(recovered.Attempts) == 0 {
		t.Fatalf("Recover result = %#v, want safe unpaired state and attempts", recovered)
	}

	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestUpgradeConfigPersistsCurrentSchemaUnderStateLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":5,"workspaces":{"saved":{"path":"/tmp/saved"}}}`), 0o600); err != nil {
		t.Fatalf("seed schema v5 config: %v", err)
	}
	if err := UpgradeConfig(context.Background(), path); err != nil {
		t.Fatalf("UpgradeConfig() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read upgraded config: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"schemaVersion": 6`)) {
		t.Fatalf("upgraded config = %s", raw)
	}
	cfg, err := (config.Store{Path: path}).Load()
	if err != nil || cfg.Workspaces["saved"].Path != "/tmp/saved" {
		t.Fatalf("upgraded config lost state: %#v error=%v", cfg, err)
	}
}

func TestAgentRuntimeRunInvokesBoundedStartupRecoveryOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	restorer := newInfrastructureRestorer(func(context.Context) (portrelay.Reconciler, error) {
		return portrelay.Reconciler{}, nil
	})
	agent := NewAgent(nil, restorer, nil)
	localSync := &recordingLocalSyncLifecycle{started: make(chan struct{}), stopped: make(chan struct{})}
	calls := 0
	runtimeAgent := &AgentRuntime{
		agent: agent, restorer: restorer, ssh: &managedSSHRuntime{}, localSync: localSync,
		startupRecover: func(recoveryCtx context.Context) error {
			calls++
			if _, ok := recoveryCtx.Deadline(); !ok {
				t.Fatal("startup recovery context has no deadline")
			}
			cancel()
			return nil
		},
	}
	if err := runtimeAgent.Run(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
	if calls != 1 {
		t.Fatalf("startup recovery calls = %d, want one bounded self-heal wave", calls)
	}
	select {
	case <-localSync.started:
	default:
		t.Fatal("local Syncthing lifecycle was not started")
	}
	select {
	case <-localSync.stopped:
	default:
		t.Fatal("local Syncthing lifecycle was not stopped")
	}
}

func TestPairingLifecycleReconcilerDiscoversWindowsRequestWithoutDesktopPolling(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleWindowsHost)
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled}); err != nil {
		t.Fatalf("enable Windows host: %v", err)
	}
	expiresAt := time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	var firstParams localapi.PairSessionParams
	calls := 0
	handler := localapi.HandlerFunc(func(_ context.Context, method localapi.Method, raw json.RawMessage) (any, error) {
		if method != localapi.MethodPairStatus {
			return nil, fmt.Errorf("background method = %q", method)
		}
		var params localapi.PairSessionParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		if calls == 0 {
			firstParams = params
		}
		calls++
		return localapi.PairingStatusResult{
			SessionID: "session-1", Code: "123456", Status: string(pairing.SessionPending), ExpiresAt: expiresAt,
			Peer: localapi.LifecyclePeer{ID: "mac", Name: "MacBook", OS: "macos"},
		}, nil
	})
	reconciler := &pairingLifecycleReconciler{machine: machine, handler: handler}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx, time.Millisecond) }()

	waitForLifecycleState(t, machine, lifecycle.StatePairing)
	cancel()
	if err := waitForTestError(t, done, "pairing lifecycle stop"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
	if pairingState := machine.Snapshot().Pairing; pairingState == nil || pairingState.SessionID != "session-1" {
		t.Fatalf("background pairing = %#v", pairingState)
	}
	if firstParams.SessionID != "" || !firstParams.ObserveOnly {
		t.Fatalf("first background params = %#v", firstParams)
	}
}

func TestPairingLifecycleReconcilerCommitsWindowsTrustWithDurableGeneration(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleWindowsHost)
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingStarted, Pairing: &lifecycle.Pairing{
		SessionID: "session-1", Peer: lifecycle.Peer{ID: "temporary-mac", Name: "Mac"}, Code: "123456",
		Status: lifecycle.PairingPending, ExpiresAt: time.Now().Add(time.Minute),
	}}); err != nil {
		t.Fatal(err)
	}
	handler := &generationPairingHandler{
		generation: "generation-one",
		result: localapi.PairingStatusResult{
			SessionID: "session-1", Status: string(pairing.SessionCompleted),
			Peer:   localapi.LifecyclePeer{ID: "temporary-mac", Name: "Mac"},
			Device: &localapi.Device{ID: "mac-one", Name: "Mac"},
		},
	}
	reconciler := &pairingLifecycleReconciler{machine: machine, handler: handler}

	if err := reconciler.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile completed Windows pairing: %v", err)
	}
	snapshot := machine.Snapshot()
	if snapshot.State != lifecycle.StateConnecting || snapshot.Pairing != nil || snapshot.TrustedPeers != 1 ||
		snapshot.Peer == nil || snapshot.Peer.ID != "mac-one" || snapshot.Peer.Generation != "generation-one" {
		t.Fatalf("completed Windows lifecycle = %#v", snapshot)
	}
}

func TestPairingLifecycleReconcilerCompletesMacPairingWithoutDesktopPolling(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted}); err != nil {
		t.Fatal(err)
	}
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingStarted, Pairing: &lifecycle.Pairing{
		SessionID: "session-1", Peer: lifecycle.Peer{ID: "windows", Name: "Windows"}, Code: "123456",
		Status: lifecycle.PairingPending, ExpiresAt: time.Now().Add(time.Minute),
	}}); err != nil {
		t.Fatal(err)
	}
	handler := localapi.HandlerFunc(func(_ context.Context, method localapi.Method, raw json.RawMessage) (any, error) {
		var params localapi.PairSessionParams
		if method != localapi.MethodPairStatus || json.Unmarshal(raw, &params) != nil ||
			params.SessionID != "session-1" || !params.ObserveOnly {
			t.Fatalf("Mac background poll = method %q params %#v", method, params)
		}
		return localapi.PairingStatusResult{
			SessionID: "session-1", Status: string(pairing.SessionApproved),
			Peer: localapi.LifecyclePeer{ID: "windows", Name: "Windows"},
		}, nil
	})
	completeCalls := 0
	afterCompleteCalls := 0
	reconciler := &pairingLifecycleReconciler{
		machine: machine, handler: handler,
		complete: func(context.Context, string) (localapi.PairingStatusResult, error) {
			completeCalls++
			return localapi.PairingStatusResult{
				SessionID: "session-1", Status: string(pairing.SessionCompleted),
				Peer:   localapi.LifecyclePeer{ID: "windows", Name: "Windows"},
				Device: &localapi.Device{ID: "trusted-windows", Name: "Windows", Address: "192.168.1.20"},
			}, nil
		},
		afterComplete: func() { afterCompleteCalls++ },
	}
	if err := reconciler.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile completed Mac pairing: %v", err)
	}
	snapshot := machine.Snapshot()
	if completeCalls != 1 || afterCompleteCalls != 1 || snapshot.State != lifecycle.StateConnecting || snapshot.Pairing != nil || snapshot.TrustedPeers != 1 ||
		snapshot.Peer == nil || snapshot.Peer.ID != "trusted-windows" {
		t.Fatalf("complete calls=%d after calls=%d Mac lifecycle=%#v", completeCalls, afterCompleteCalls, snapshot)
	}
}

func TestPairingLifecycleReconcilerPreservesDurableCompletionWhenStopWinsRevision(t *testing.T) {
	for _, eventType := range []lifecycle.EventType{lifecycle.EventPauseRequested, lifecycle.EventQuitRequested} {
		t.Run(string(eventType), func(t *testing.T) {
			machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
			if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled}); err != nil {
				t.Fatal(err)
			}
			if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted}); err != nil {
				t.Fatal(err)
			}
			if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingStarted, Pairing: &lifecycle.Pairing{
				SessionID: "session-1", Peer: lifecycle.Peer{ID: "windows", Name: "Windows"}, Code: "123456",
				Status: lifecycle.PairingPending, ExpiresAt: time.Now().Add(time.Minute),
			}}); err != nil {
				t.Fatal(err)
			}
			handler := localapi.HandlerFunc(func(context.Context, localapi.Method, json.RawMessage) (any, error) {
				return localapi.PairingStatusResult{
					SessionID: "session-1", Status: string(pairing.SessionApproved),
					Peer: localapi.LifecyclePeer{ID: "windows", Name: "Windows"},
				}, nil
			})
			completionStarted := make(chan struct{})
			releaseCompletion := make(chan struct{})
			resolvedSession := ""
			cancelContextErr := errors.New("phase-aware cancellation was not called")
			rolledBack := ""
			afterCompleteCalls := 0
			reconciler := &pairingLifecycleReconciler{
				machine: machine, handler: handler,
				complete: func(context.Context, string) (localapi.PairingStatusResult, error) {
					close(completionStarted)
					<-releaseCompletion
					return localapi.PairingStatusResult{
						SessionID: "session-1", Status: string(pairing.SessionCompleted),
						Peer:   localapi.LifecyclePeer{ID: "windows", Name: "Windows"},
						Device: &localapi.Device{ID: "trusted-windows", Name: "Windows"},
					}, nil
				},
				rollback: func(rollbackCtx context.Context, deviceID string) error {
					rolledBack = deviceID
					return nil
				},
				cancelCurrent: func(cancelCtx context.Context, sessionID string) (localapi.PairingStatusResult, error) {
					resolvedSession = sessionID
					cancelContextErr = cancelCtx.Err()
					return localapi.PairingStatusResult{
						SessionID: sessionID, Status: string(pairing.SessionCompleted),
						Device: &localapi.Device{ID: "trusted-windows", Name: "Windows"},
					}, nil
				},
				afterComplete: func() { afterCompleteCalls++ },
			}
			reconcileCtx, cancelReconcile := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- reconciler.reconcile(reconcileCtx) }()
			waitForTestSignal(t, completionStarted, "protected pairing completion")
			if _, err := machine.Apply(lifecycle.Event{Type: eventType}); err != nil {
				t.Fatalf("stop while completion is pending: %v", err)
			}
			cancelReconcile()
			close(releaseCompletion)
			if err := waitForTestError(t, done, "pairing completion rollback"); err != nil {
				t.Fatalf("reconcile after lost lease error = %v", err)
			}
			snapshot := machine.Snapshot()
			if resolvedSession != "session-1" || cancelContextErr != nil || rolledBack != "" || afterCompleteCalls != 0 ||
				snapshot.State != lifecycle.StateStopping || snapshot.TrustedPeers != 0 {
				t.Fatalf("resolved=%q context error=%v rollback=%q after calls=%d lifecycle=%#v", resolvedSession, cancelContextErr, rolledBack, afterCompleteCalls, snapshot)
			}
		})
	}
}

func TestPairingLifecycleReconcilerExpiresOfflineSessionLocally(t *testing.T) {
	for _, test := range []struct {
		name          string
		nowOffset     time.Duration
		wantCalls     int
		wantAbandoned string
		wantState     lifecycle.State
		wantError     bool
	}{
		{name: "before expiry", nowOffset: -time.Second, wantCalls: 1, wantState: lifecycle.StatePairing, wantError: true},
		{name: "after expiry", nowOffset: time.Second, wantAbandoned: "session-1", wantState: lifecycle.StateClientReady},
	} {
		t.Run(test.name, func(t *testing.T) {
			machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
			if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled}); err != nil {
				t.Fatal(err)
			}
			if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted}); err != nil {
				t.Fatal(err)
			}
			expiresAt := time.Now().Add(time.Minute)
			if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingStarted, Pairing: &lifecycle.Pairing{
				SessionID: "session-1", Peer: lifecycle.Peer{ID: "windows", Name: "Windows"}, Code: "123456",
				Status: lifecycle.PairingPending, ExpiresAt: expiresAt,
			}}); err != nil {
				t.Fatal(err)
			}
			statusCalls := 0
			abandoned := ""
			reconciler := &pairingLifecycleReconciler{
				machine: machine,
				handler: localapi.HandlerFunc(func(context.Context, localapi.Method, json.RawMessage) (any, error) {
					statusCalls++
					return nil, unavailable("peer is offline")
				}),
				now:     func() time.Time { return expiresAt.Add(test.nowOffset) },
				abandon: func(sessionID string) { abandoned = sessionID },
			}
			err := reconciler.reconcile(context.Background())
			if (err != nil) != test.wantError {
				t.Fatalf("local expiry error = %v, wantError=%t", err, test.wantError)
			}
			snapshot := machine.Snapshot()
			if statusCalls != test.wantCalls || abandoned != test.wantAbandoned || snapshot.State != test.wantState ||
				(snapshot.Pairing != nil) != (test.wantState == lifecycle.StatePairing) {
				t.Fatalf("status calls=%d abandoned=%q lifecycle=%#v", statusCalls, abandoned, snapshot)
			}
		})
	}
}

func TestPairingLifecycleReconcilerDiscardsResultAfterSessionRevisionChanges(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted}); err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().Add(time.Minute)
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingStarted, Pairing: &lifecycle.Pairing{
		SessionID: "session-1", Peer: lifecycle.Peer{ID: "windows", Name: "Windows"}, Code: "123456",
		Status: lifecycle.PairingPending, ExpiresAt: expiresAt,
	}}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	handler := localapi.HandlerFunc(func(context.Context, localapi.Method, json.RawMessage) (any, error) {
		close(started)
		<-release
		return localapi.PairingStatusResult{
			SessionID: "session-1", Status: string(pairing.SessionCompleted),
			Peer:   localapi.LifecyclePeer{ID: "windows", Name: "Windows"},
			Device: &localapi.Device{ID: "trusted-windows", Name: "Windows"},
		}, nil
	})
	reconciler := &pairingLifecycleReconciler{machine: machine, handler: handler}
	done := make(chan error, 1)
	go func() { done <- reconciler.reconcile(context.Background()) }()
	waitForTestSignal(t, started, "pairing status request")
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingCancelled}); err != nil {
		t.Fatalf("cancel pairing while status is pending: %v", err)
	}
	close(release)
	if err := waitForTestError(t, done, "stale pairing result"); err != nil {
		t.Fatalf("reconcile stale result error = %v", err)
	}
	snapshot := machine.Snapshot()
	if snapshot.Pairing != nil || snapshot.TrustedPeers != 0 || snapshot.State != lifecycle.StateClientReady {
		t.Fatalf("stale result changed lifecycle = %#v", snapshot)
	}
}

func TestPendingRevocationCleanupDoesNotBlockPairingReconciliation(t *testing.T) {
	machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted}); err != nil {
		t.Fatal(err)
	}
	if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingStarted, Pairing: &lifecycle.Pairing{
		SessionID: "session-1", Peer: lifecycle.Peer{ID: "windows", Name: "Windows"}, Code: "123456",
		Status: lifecycle.PairingPending, ExpiresAt: time.Now().Add(time.Minute),
	}}); err != nil {
		t.Fatal(err)
	}
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	statusCalled := make(chan struct{})
	reconciler := &pairingLifecycleReconciler{
		machine: machine,
		handler: localapi.HandlerFunc(func(context.Context, localapi.Method, json.RawMessage) (any, error) {
			close(statusCalled)
			return localapi.PairingStatusResult{
				SessionID: "session-1", Status: string(pairing.SessionPending), ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339Nano),
			}, nil
		}),
		cleanup: func(context.Context) error {
			close(cleanupStarted)
			<-releaseCleanup
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx, time.Hour) }()
	waitForTestSignal(t, cleanupStarted, "pending revocation cleanup")
	waitForTestSignal(t, statusCalled, "pairing reconciliation while cleanup is blocked")
	close(releaseCleanup)
	cancel()
	if err := waitForTestError(t, done, "pairing reconciler stop"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
}

func waitForLifecycleState(t *testing.T, machine *lifecycle.Machine, want lifecycle.State) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if machine.Snapshot().State == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("lifecycle state = %q, want %q", machine.Snapshot().State, want)
}

func TestAgentRuntimePreparesLocalSyncBeforePublishingRunning(t *testing.T) {
	restorer := newInfrastructureRestorer(func(context.Context) (portrelay.Reconciler, error) {
		return portrelay.Reconciler{}, nil
	})
	localSync := &recordingLocalSyncLifecycle{
		started: make(chan struct{}), stopped: make(chan struct{}),
		prepareStarted: make(chan struct{}), prepareRelease: make(chan struct{}),
	}
	runtimeAgent := &AgentRuntime{
		agent: NewAgent(nil, restorer, nil), restorer: restorer, localSync: localSync,
		startupRecover: func(context.Context) error { return nil },
	}
	startDone := make(chan error, 1)
	go func() { startDone <- runtimeAgent.Start(context.Background(), lifecycle.RoleMacClient) }()

	select {
	case <-localSync.prepareStarted:
	case <-time.After(time.Second):
		t.Fatal("local sync preparation did not start")
	}
	select {
	case err := <-startDone:
		t.Fatalf("Start() returned before local sync preparation completed: %v", err)
	default:
	}
	select {
	case <-localSync.started:
		t.Fatal("local sync child started before preparation completed")
	default:
	}

	close(localSync.prepareRelease)
	if err := <-startDone; err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-localSync.started:
	case <-time.After(time.Second):
		t.Fatal("local sync child did not start after preparation")
	}
	if err := runtimeAgent.Stop(context.Background(), lifecycle.StopPause); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestAgentRuntimePreparationFailureStartsNoSessionChildren(t *testing.T) {
	restorer := newInfrastructureRestorer(func(context.Context) (portrelay.Reconciler, error) {
		return portrelay.Reconciler{}, nil
	})
	wantErr := &localSyncIdentityBlockedError{cause: syncer.ErrIdentityCorrupt}
	localSync := &recordingLocalSyncLifecycle{
		started: make(chan struct{}), stopped: make(chan struct{}), prepareErr: wantErr,
	}
	recoveryCalls := 0
	runtimeAgent := &AgentRuntime{
		agent: NewAgent(nil, restorer, nil), restorer: restorer, localSync: localSync,
		startupRecover: func(context.Context) error { recoveryCalls++; return nil },
	}
	if err := runtimeAgent.Start(context.Background(), lifecycle.RoleMacClient); !errors.Is(err, wantErr) {
		t.Fatalf("Start() error = %v, want %v", err, wantErr)
	}
	runtimeAgent.sessionMu.Lock()
	running := runtimeAgent.sessionRunning
	runtimeAgent.sessionMu.Unlock()
	if running || recoveryCalls != 0 {
		t.Fatalf("failed preparation running=%t recoveryCalls=%d", running, recoveryCalls)
	}
	select {
	case <-localSync.started:
		t.Fatal("local sync child started after failed preparation")
	default:
	}
}

func TestAgentRuntimeStartAndStopOwnOneSessionLifecycle(t *testing.T) {
	restorer := newInfrastructureRestorer(func(context.Context) (portrelay.Reconciler, error) {
		return portrelay.Reconciler{}, nil
	})
	agent := NewAgent(nil, restorer, nil)
	localSync := &recordingLocalSyncLifecycle{started: make(chan struct{}), stopped: make(chan struct{})}
	runtimeAgent := &AgentRuntime{
		agent: agent, restorer: restorer, localSync: localSync,
		startupRecover: func(context.Context) error { return nil },
	}
	if err := runtimeAgent.Start(context.Background(), lifecycle.RoleMacClient); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtimeAgent.Start(context.Background(), lifecycle.RoleMacClient); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	select {
	case <-localSync.started:
	case <-time.After(time.Second):
		t.Fatal("owned local sync runtime did not start")
	}
	if err := runtimeAgent.Stop(context.Background(), lifecycle.StopPause); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case <-localSync.stopped:
	case <-time.After(time.Second):
		t.Fatal("owned local sync runtime did not stop")
	}
	select {
	case err := <-runtimeAgent.Done():
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Done() error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Done() did not report the completed session")
	}
}

func TestAgentRuntimeStopAbandonsPendingPairingSecrets(t *testing.T) {
	for _, reason := range []lifecycle.StopReason{lifecycle.StopPause, lifecycle.StopQuit} {
		t.Run(string(reason), func(t *testing.T) {
			machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
			if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled}); err != nil {
				t.Fatal(err)
			}
			if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted}); err != nil {
				t.Fatal(err)
			}
			if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingStarted, Pairing: &lifecycle.Pairing{
				SessionID: "session-1", Peer: lifecycle.Peer{ID: "windows", Name: "Windows"}, Code: "123456",
				Status: lifecycle.PairingPending, ExpiresAt: time.Now().Add(time.Minute),
			}}); err != nil {
				t.Fatal(err)
			}
			wait := make(chan struct{})
			close(wait)
			abandoned := ""
			runtimeAgent := &AgentRuntime{
				sessionCancel: func() {}, sessionWait: wait, sessionErr: context.Canceled,
				pairingRuntime: &pairingLifecycleReconciler{
					machine: machine,
					abandon: func(sessionID string) { abandoned = sessionID },
				},
			}
			if err := runtimeAgent.Stop(context.Background(), reason); err != nil {
				t.Fatalf("Stop(%s) error = %v", reason, err)
			}
			if abandoned != "session-1" {
				t.Fatalf("Stop(%s) abandoned session = %q", reason, abandoned)
			}
		})
	}
}

func TestAgentRuntimePauseAndQuitShareCurrentPairingCancellation(t *testing.T) {
	for _, reason := range []lifecycle.StopReason{lifecycle.StopPause, lifecycle.StopQuit} {
		t.Run(string(reason), func(t *testing.T) {
			root := t.TempDir()
			store := config.Store{Path: filepath.Join(root, "config.json")}
			transport := &blockingCancelPairingTransport{
				runtimePairingTransport: &runtimePairingTransport{hostKey: testAuthorizedKey(t), status: pairing.SessionPending},
				started:                 make(chan struct{}), release: make(chan struct{}),
			}
			coordinator := newMacPairingCoordinator(macPairingOptions{
				Store: store, Secrets: credentials.NewMemoryStore(), Transport: transport, Docker: &runtimeDockerExecutor{},
				DockerCLI: "docker-real", DockerContext: "remote-docker",
				ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
			})
			started, err := coordinator.Start(context.Background(), "windows-peer")
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			machine := newLifecycleMachine(t, lifecycle.RoleMacClient)
			if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled}); err != nil {
				t.Fatal(err)
			}
			if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventSearchStarted}); err != nil {
				t.Fatal(err)
			}
			if _, err := machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingStarted, Pairing: &lifecycle.Pairing{
				SessionID: started.SessionID, Peer: lifecycle.Peer{ID: "windows", Name: "Windows"}, Code: started.Code,
				Status: lifecycle.PairingPending, ExpiresAt: time.Now().Add(time.Minute),
			}}); err != nil {
				t.Fatal(err)
			}
			cancelDone := make(chan error, 1)
			go func() {
				_, cancelErr := coordinator.CancelCurrent(context.Background(), started.SessionID)
				cancelDone <- cancelErr
			}()
			waitForTestSignal(t, transport.started, "remote pairing cancellation")
			wait := make(chan struct{})
			close(wait)
			abandoned := 0
			runtimeAgent := &AgentRuntime{
				sessionCancel: func() {}, sessionWait: wait, sessionErr: context.Canceled,
				pairingRuntime: &pairingLifecycleReconciler{
					machine: machine, cancelCurrent: coordinator.CancelCurrent,
					abandon: func(string) { abandoned++ },
				},
			}
			if err := runtimeAgent.Stop(context.Background(), reason); err != nil {
				t.Fatalf("Stop(%s) error = %v", reason, err)
			}
			if abandoned != 0 || transport.cancelCalls.Load() != 1 {
				t.Fatalf("Stop(%s) abandon=%d remote cancel calls=%d", reason, abandoned, transport.cancelCalls.Load())
			}
			close(transport.release)
			if err := waitForTestError(t, cancelDone, "owned pairing cancellation"); err != nil {
				t.Fatalf("owned CancelCurrent() error = %v", err)
			}
		})
	}
}

func TestDesktopControllerFailedPausedConnectJoinsStartupRecoveryBeforeForget(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		ActiveDevice:  "saved",
		Devices: map[string]config.Device{
			"saved": {Name: "Saved Windows", Address: "192.168.1.20", SSHPort: 49222},
		},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	secrets := credentials.NewMemoryStore()
	if err := secrets.Put("saved", sshtransport.SSHPrivateKeyCredential, []byte("private-key")); err != nil {
		t.Fatalf("seed private key: %v", err)
	}
	recoveryBlocked := make(chan struct{})
	recoveryCancelled := make(chan struct{})
	releaseRecovery := make(chan struct{})
	recoveryExited := make(chan struct{})
	sshConfigPath := filepath.Join(root, "ssh_config")
	sshRuntime := &managedSSHRuntime{
		store: store, secrets: secrets,
		sshConfigPath:   sshConfigPath,
		knownHostsPath:  filepath.Join(root, "known_hosts"),
		agentSocketPath: filepath.Join(root, "agent", "ssh-agent.sock"),
		controlDir:      filepath.Join(root, "control"),
		start: func(ctx context.Context, _ string, _ []byte) (managedSSHAgent, error) {
			close(recoveryBlocked)
			<-ctx.Done()
			close(recoveryCancelled)
			<-releaseRecovery
			return nil, ctx.Err()
		},
	}
	restorer := newInfrastructureRestorer(func(ctx context.Context) (portrelay.Reconciler, error) {
		if err := sshRuntime.Ensure(ctx); err != nil {
			return portrelay.Reconciler{}, err
		}
		return portrelay.Reconciler{}, nil
	})
	agent := NewAgent(nil, restorer, nil)
	runtimeAgent := &AgentRuntime{
		agent: agent, restorer: restorer, ssh: sshRuntime,
		startupRecover: func(ctx context.Context) error {
			defer close(recoveryExited)
			return agent.Reconnect(ctx)
		},
	}
	machine, err := lifecycle.NewMachine(lifecycle.RoleMacClient, "Mac", lifecycle.WithTrustedPeer(lifecycle.Peer{ID: "saved", Name: "Saved Windows"}))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	supervisor, _ := NewSupervisor(machine, runtimeAgent)
	foregroundErr := errors.New("injected foreground reconnect failure")
	cleanupCalls := 0
	fallback := localapi.HandlerFunc(func(_ context.Context, method localapi.Method, _ json.RawMessage) (any, error) {
		switch method {
		case localapi.MethodConnect:
			select {
			case <-recoveryBlocked:
				if got := machine.Snapshot(); got.State != lifecycle.StateConnecting || !got.ActionInProgress {
					return nil, errors.New("startup recovery ran before lifecycle connection reservation")
				}
				return nil, foregroundErr
			case <-time.After(time.Second):
				return nil, errors.New("startup recovery did not reach the pre-write barrier")
			}
		case localapi.MethodUnpair:
			cleanupCalls++
			return nil, nil
		default:
			return nil, errors.New("unexpected fallback method")
		}
	})
	controller, _ := NewDesktopController(supervisor, fallback)
	connectDone := make(chan error, 1)
	go func() {
		_, err := controller.Handle(context.Background(), localapi.MethodConnect, nil)
		connectDone <- err
	}()
	waitForTestSignal(t, recoveryCancelled, "startup recovery cancellation")
	if got := machine.Snapshot(); got.State != lifecycle.StateStopping || !got.ActionInProgress {
		t.Fatalf("snapshot while failed connect joins recovery = %#v", got)
	}
	if _, err := controller.Handle(context.Background(), localapi.MethodForgetDevice, json.RawMessage(`{"device_id":"saved","local_only":true}`)); err == nil {
		t.Fatal("ForgetDevice while connection abort joins recovery error = nil")
	}
	if cleanupCalls != 0 {
		t.Fatalf("forget cleanup ran before connection abort joined: calls=%d", cleanupCalls)
	}
	select {
	case err := <-connectDone:
		t.Fatalf("Connect returned before startup recovery joined: %v", err)
	default:
	}
	close(releaseRecovery)
	if err := waitForTestError(t, connectDone, "failed connect completion"); !errors.Is(err, foregroundErr) {
		t.Fatalf("Connect error = %v, want foreground failure", err)
	}
	waitForTestSignal(t, recoveryExited, "startup recovery exit")
	if got := machine.Snapshot(); got.State != lifecycle.StatePaused || got.ActionInProgress || !machine.Allowed(lifecycle.CommandForget) {
		t.Fatalf("snapshot after failed connect abort = %#v", got)
	}
	if _, err := os.Stat(sshConfigPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed ssh_config after joined abort: %v", err)
	}
	if _, err := controller.Handle(context.Background(), localapi.MethodForgetDevice, json.RawMessage(`{"device_id":"saved","local_only":true}`)); err != nil {
		t.Fatalf("ForgetDevice after joined connection abort error = %v", err)
	}
	if cleanupCalls != 1 {
		t.Fatalf("forget cleanup calls after joined abort = %d, want one", cleanupCalls)
	}
}

func TestAgentRuntimeStopCleansOnlyManagedWindowsRuntime(t *testing.T) {
	restorer := newInfrastructureRestorer(func(context.Context) (portrelay.Reconciler, error) {
		return portrelay.Reconciler{}, nil
	})
	stopper := &recordingWindowsRuntimeStopper{}
	runtimeAgent := &AgentRuntime{
		agent: NewAgent(nil, restorer, nil), restorer: restorer, windowsStopper: stopper,
		startupRecover: func(context.Context) error { return nil },
	}
	if err := runtimeAgent.Start(context.Background(), lifecycle.RoleWindowsHost); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtimeAgent.Stop(context.Background(), lifecycle.StopQuit); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if stopper.calls != 1 {
		t.Fatalf("managed Windows stop calls = %d, want 1", stopper.calls)
	}
}

type recordingWindowsRuntimeStopper struct{ calls int }

func (s *recordingWindowsRuntimeStopper) StopManagedRuntime(context.Context) (windowsbridge.StopReport, error) {
	s.calls++
	return windowsbridge.StopReport{ContainersStopped: true, TargetStopped: true, DistroTerminated: true}, nil
}

func TestAgentRuntimeRunsWindowsBridgeAlongsideRuntimeIdentityLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	restorer := newInfrastructureRestorer(func(context.Context) (portrelay.Reconciler, error) {
		return portrelay.Reconciler{}, nil
	})
	agent := NewAgent(nil, restorer, nil)
	identity := &recordingLocalSyncLifecycle{started: make(chan struct{}), stopped: make(chan struct{})}
	bridge := &recordingLocalSyncLifecycle{started: make(chan struct{}), stopped: make(chan struct{})}
	runtimeAgent := &AgentRuntime{
		agent: agent, restorer: restorer, localSync: identity, windowsBridge: bridge,
		startupRecover: func(context.Context) error {
			select {
			case <-identity.started:
			case <-time.After(time.Second):
				t.Fatal("runtime identity lifecycle did not start")
			}
			select {
			case <-bridge.started:
			case <-time.After(time.Second):
				t.Fatal("Windows bridge lifecycle did not start")
			}
			cancel()
			return nil
		},
	}
	if err := runtimeAgent.Run(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
	for name, stopped := range map[string]<-chan struct{}{
		"runtime identity": identity.stopped,
		"Windows bridge":   bridge.stopped,
	} {
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatalf("%s lifecycle did not stop", name)
		}
	}
}

type recordingLocalSyncLifecycle struct {
	started        chan struct{}
	stopped        chan struct{}
	prepareStarted chan struct{}
	prepareRelease chan struct{}
	prepareErr     error
	once           sync.Once
}

func (r *recordingLocalSyncLifecycle) Prepare(ctx context.Context) (localSyncPreparation, error) {
	if r.prepareStarted != nil {
		close(r.prepareStarted)
	}
	if r.prepareRelease != nil {
		select {
		case <-ctx.Done():
			return localSyncPreparation{}, ctx.Err()
		case <-r.prepareRelease:
		}
	}
	return localSyncPreparation{}, r.prepareErr
}

func (r *recordingLocalSyncLifecycle) Run(ctx context.Context, _ time.Duration) error {
	r.once.Do(func() { close(r.started) })
	<-ctx.Done()
	close(r.stopped)
	return ctx.Err()
}

func TestStartupRecoveryUsesTypedDiagnosticsOnlyOnWindows(t *testing.T) {
	reconnects := 0
	selfHeals := 0
	reconnect := func(context.Context) error { reconnects++; return nil }
	selfHeal := func(context.Context) error { selfHeals++; return nil }

	if err := selectStartupRecovery("windows", reconnect, selfHeal)(context.Background()); err != nil {
		t.Fatalf("Windows startup recovery error = %v", err)
	}
	if reconnects != 0 || selfHeals != 1 {
		t.Fatalf("Windows startup calls reconnect=%d self-heal=%d, want diagnostics self-heal only", reconnects, selfHeals)
	}
	if err := selectStartupRecovery("darwin", reconnect, selfHeal)(context.Background()); err != nil {
		t.Fatalf("Darwin startup recovery error = %v", err)
	}
	if reconnects != 1 || selfHeals != 1 {
		t.Fatalf("Darwin startup calls reconnect=%d self-heal=%d, want reconnect only", reconnects, selfHeals)
	}
}

func TestProductionAgentControllerPreparesDockerWithoutExecutingUserCommand(t *testing.T) {
	preparer := &recordingDockerPreparer{}
	controller := &productionAgentController{dockerPreparer: preparer}
	raw := []byte(`{"bind_sources":["/Users/demo/project"],"working_directory":"/Users/demo/project"}`)

	result, err := controller.Handle(context.Background(), localapi.MethodPrepareDocker, raw)
	if err != nil {
		t.Fatalf("PrepareDocker error = %v", err)
	}
	if result != (localapi.PrepareDockerResult{Ready: true}) {
		t.Fatalf("PrepareDocker result = %#v", result)
	}
	if !reflect.DeepEqual(preparer.params.BindSources, []string{"/Users/demo/project"}) ||
		preparer.params.WorkingDirectory != "/Users/demo/project" {
		t.Fatalf("preparer params = %#v", preparer.params)
	}
}

type recordingDockerPreparer struct {
	params localapi.PrepareDockerParams
}

func (p *recordingDockerPreparer) Prepare(_ context.Context, params localapi.PrepareDockerParams) error {
	p.params = params
	return nil
}

func TestProductionDiagnosticsReturnsOrderedSafeChecks(t *testing.T) {
	options := productionDiagnosticsOptions{
		Observe: func(context.Context) AgentStatus {
			return AgentStatus{State: AgentReady, Message: "Bearer not-a-secret-in-output"}
		},
		Platform: "darwin",
	}
	setTestDiagnosticChecks(&options, func(context.Context) error { return nil })
	checks := newProductionDiagnosticsWithOptions(options).Doctor(context.Background()).Checks
	wantNames := []string{
		"lan_reachability", "tunnel_identity", "tunnel_session",
		"local_relays", "docker_channel", "sync_channel", "managed_wsl",
	}
	if len(checks) != len(wantNames) {
		t.Fatalf("check count = %d, want %d", len(checks), len(wantNames))
	}
	for index, want := range wantNames {
		if checks[index].Name != want {
			t.Fatalf("check[%d].Name = %q, want %q", index, checks[index].Name, want)
		}
		if strings.Contains(checks[index].Message, "not-a-secret-in-output") {
			t.Fatalf("check[%d] leaked observer message: %#v", index, checks[index])
		}
	}
	for index, check := range checks {
		if !check.OK || check.Message != "" {
			t.Fatalf("check[%d] = %#v, want production-ready operation", index, check)
		}
	}
}

func TestMacPairingCoordinatorPersistsPinnedDeviceAndRevokesBeforeLocalRemoval(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	transport := &runtimePairingTransport{hostKey: testAuthorizedKey(t)}
	docker := &runtimeDockerExecutor{}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: docker,
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID:  func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:   filepath.Join(root, "ssh_config"),
		ManagedSSHRoot:  testManagedSSHRoot(t, root),
		KnownHostsPath:  filepath.Join(root, "known_hosts"),
		AgentSocketPath: filepath.Join(root, "ssh-agent.sock"),
		ControlDir:      filepath.Join(root, "control"),
	})
	candidates, err := coordinator.Candidates(context.Background())
	if err != nil {
		t.Fatalf("Candidates() error = %v", err)
	}
	if want := []localapi.PairingCandidate{{ID: "windows-peer", Name: "Dev PC", Unverified: true, Available: true}}; !reflect.DeepEqual(candidates.Candidates, want) {
		t.Fatalf("candidates = %#v, want %#v", candidates.Candidates, want)
	}

	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if started.SessionID == "" || len(started.Code) != 6 {
		t.Fatalf("pair start = %#v", started)
	}
	status, err := coordinator.Status(context.Background(), started.SessionID)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.Status != string(pairing.SessionCompleted) || status.Device == nil || status.Device.ID == "" || status.Device.Address != "192.168.1.20" {
		t.Fatalf("pairing status = %#v", status)
	}
	confirmed := *status.Device
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	device := cfg.Devices[confirmed.ID]
	if cfg.ActiveDevice != confirmed.ID || device.SSHHostPublicKey != transport.hostKey ||
		device.SyncthingDeviceID != "WINDOWS-SYNC" || device.ClientDeviceID != "LOCAL-SYNC" ||
		device.SSHPort != 49222 || device.SyncPort != 49220 || device.TunnelPort != tunnel.TunnelPort ||
		device.TransportVersion != tunnel.CurrentTransportVersion || device.TunnelPeerPublicKey == "" || device.RevocationCredentialOwner == "" {
		t.Fatalf("persisted device = %#v config=%#v", device, cfg)
	}
	privateKey, err := secrets.Get(confirmed.ID, sshtransport.SSHPrivateKeyCredential)
	if err != nil || len(privateKey) == 0 {
		t.Fatalf("stored private key length=%d error=%v", len(privateKey), err)
	}
	encodedTunnelIdentity, err := secrets.Get(confirmed.ID, tunnel.IdentityCredential)
	if err != nil {
		t.Fatalf("stored tunnel identity error = %v", err)
	}
	if _, err := tunnel.IdentityFromPKCS8(encodedTunnelIdentity); err != nil {
		t.Fatalf("stored tunnel identity is invalid: %v", err)
	}
	if proof, err := secrets.Get(device.RevocationCredentialOwner, revocationProofCredential); err != nil || len(proof) != pairing.RevocationProofSize {
		t.Fatalf("stored revocation proof length=%d error=%v", len(proof), err)
	}
	knownHosts, _ := os.ReadFile(filepath.Join(root, "known_hosts"))
	if !strings.Contains(string(knownHosts), transport.hostKey) {
		t.Fatalf("known_hosts = %q", knownHosts)
	}
	if len(docker.calls) != 4 {
		t.Fatalf("Docker context calls = %#v", docker.calls)
	}

	controller := &productionAgentController{pairing: coordinator}
	if err := controller.rollbackCompletedPairing(context.Background(), confirmed.ID); err != nil {
		t.Fatalf("rollbackCompletedPairing() error = %v", err)
	}
	if transport.revoked != device.ClientDeviceID {
		t.Fatalf("remote revoked device = %q, want %q", transport.revoked, device.ClientDeviceID)
	}
	cfg, _ = store.Load()
	if cfg.ActiveDevice != "" || len(cfg.Devices) != 0 {
		t.Fatalf("config after unpair = %#v", cfg)
	}
	if _, err := secrets.Get(confirmed.ID, sshtransport.SSHPrivateKeyCredential); !errors.Is(err, credentials.ErrNotFound) {
		t.Fatalf("private key after unpair error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "ssh_config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed SSH config after unpair error = %v", err)
	}
	knownHosts, err = os.ReadFile(filepath.Join(root, "known_hosts"))
	if err != nil || len(knownHosts) != 0 {
		t.Fatalf("known_hosts after unpair = %q, error = %v", knownHosts, err)
	}
}

func TestMacPairingCompletionCancellationRollsBackConfirmedProductionArtifacts(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	transport := &runtimePairingTransport{hostKey: testAuthorizedKey(t)}
	transport.afterConfirm = func() {
		transport.status = pairing.SessionCompleted
		cancelRequest()
	}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: contextCheckingDockerExecutor{},
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:  filepath.Join(root, "ssh_config"), ManagedSSHRoot: testManagedSSHRoot(t, root),
		KnownHostsPath: filepath.Join(root, "known_hosts"), AgentSocketPath: filepath.Join(root, "ssh-agent.sock"),
		ControlDir: filepath.Join(root, "control"),
	})
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	journal, err := store.Load()
	if err != nil || len(journal.PendingRevocations) != 1 {
		t.Fatalf("write-ahead rollback journal = %#v error=%v", journal, err)
	}
	for _, pending := range journal.PendingRevocations {
		if pending.CleanupRequested || pending.SessionID != started.SessionID || pending.Device.RevocationCredentialOwner == "" {
			t.Fatalf("write-ahead rollback record = %#v", pending)
		}
		if _, err := secrets.Get(pending.Device.RevocationCredentialOwner, revocationProofCredential); err != nil {
			t.Fatalf("write-ahead rollback proof error = %v", err)
		}
	}
	if _, err := coordinator.Status(requestCtx, started.SessionID); err == nil {
		t.Fatal("Status() error = nil after completion context cancellation")
	}
	if transport.revokeCalls != 2 {
		t.Fatalf("remote revoke calls = %d, want pre-fence and post-fence revoke", transport.revokeCalls)
	}
	cfg, err := store.Load()
	if err != nil || cfg.ActiveDevice != "" || len(cfg.Devices) != 0 || len(cfg.PendingRevocations) != 0 {
		t.Fatalf("config after transactional rollback = %#v error=%v", cfg, err)
	}
	deviceID, err := pairedRemoteDeviceID(transport.hostKey)
	if err != nil {
		t.Fatalf("paired device ID: %v", err)
	}
	for _, credential := range []string{sshtransport.SSHPrivateKeyCredential, tunnel.IdentityCredential} {
		if _, err := secrets.Get(deviceID, credential); !errors.Is(err, credentials.ErrNotFound) {
			t.Fatalf("credential %q survived rollback: %v", credential, err)
		}
	}
	for _, pending := range journal.PendingRevocations {
		if _, err := secrets.Get(pending.Device.RevocationCredentialOwner, revocationProofCredential); !errors.Is(err, credentials.ErrNotFound) {
			t.Fatalf("rollback proof survived rollback: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "ssh_config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed SSH config survived rollback: %v", err)
	}
}

func TestMacPairingOfflineRevocationRetriesAfterRestartWithoutBlockingOtherDevices(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	transport := &runtimePairingTransport{
		hostKey: testAuthorizedKey(t), revokeErrors: []error{errors.New("windows offline"), nil},
	}
	options := macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: &runtimeDockerExecutor{},
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:  filepath.Join(root, "ssh_config"), ManagedSSHRoot: testManagedSSHRoot(t, root),
		KnownHostsPath: filepath.Join(root, "known_hosts"), AgentSocketPath: filepath.Join(root, "ssh-agent.sock"),
		ControlDir: filepath.Join(root, "control"),
	}
	coordinator := newMacPairingCoordinator(options)
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	status, err := coordinator.Status(context.Background(), started.SessionID)
	if err != nil || status.Device == nil {
		t.Fatalf("complete pairing status=%#v error=%v", status, err)
	}
	deviceID := status.Device.ID
	pairedConfig, err := store.Load()
	if err != nil {
		t.Fatalf("load paired config: %v", err)
	}
	proofOwner := pairedConfig.Devices[deviceID].RevocationCredentialOwner
	if err := coordinator.Unpair(context.Background(), deviceID, false); err != nil {
		t.Fatalf("Unpair() queued offline revoke error = %v", err)
	}
	cfg, err := store.Load()
	if err != nil || cfg.ActiveDevice != "" || len(cfg.Devices) != 0 || len(cfg.PendingRevocations) != 1 {
		t.Fatalf("config while revoke is pending = %#v error=%v", cfg, err)
	}
	for _, pending := range cfg.PendingRevocations {
		if pending.RemoteRevoked || !pending.DockerRestored || !pending.LocalCleaned || pending.Finished || pending.LocalDeviceID != "" {
			t.Fatalf("durable offline cleanup stages = %#v", pending)
		}
	}
	controller := &productionAgentController{store: store, pairing: coordinator}
	devices, err := controller.listDevices()
	if err != nil || len(devices.Devices) != 0 {
		t.Fatalf("visible devices while revoke pending = %#v error=%v", devices, err)
	}
	candidates, err := coordinator.Candidates(context.Background())
	if err != nil || len(candidates.Candidates) == 0 || candidates.Candidates[0].Trusted {
		t.Fatalf("other pairing candidates blocked by pending revoke: %#v error=%v", candidates, err)
	}
	restarted := newMacPairingCoordinator(options)
	if err := restarted.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("ReconcilePendingRevocations() error = %v", err)
	}
	cfg, err = store.Load()
	if err != nil || len(cfg.PendingRevocations) != 0 || transport.revokeCalls != 2 {
		t.Fatalf("config after restarted revoke = %#v calls=%d error=%v", cfg, transport.revokeCalls, err)
	}
	if _, err := secrets.Get(proofOwner, revocationProofCredential); !errors.Is(err, credentials.ErrNotFound) {
		t.Fatalf("revocation proof survived acknowledged cleanup: %v", err)
	}
}

func TestPendingRevocationKeepsProofUntilRemoteResultIsDurable(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	generation := "generation-1"
	proofOwner := "pairing-generation-1"
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		PendingRevocations: map[string]config.PendingRevocation{generation: {
			Generation: generation, CleanupRequested: true,
			Device: config.Device{ClientDeviceID: "LOCAL-SYNC", RevocationCredentialOwner: proofOwner},
		}},
	}); err != nil {
		t.Fatalf("seed pending revocation: %v", err)
	}
	secrets := credentials.NewMemoryStore()
	proof := bytes.Repeat([]byte{7}, pairing.RevocationProofSize)
	if err := secrets.Put(proofOwner, revocationProofCredential, proof); err != nil {
		t.Fatalf("seed revocation proof: %v", err)
	}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: &runtimePairingTransport{},
		SaveConfig: func(config.Config) error { return errors.New("injected journal save failure") },
	})
	if err := coordinator.ReconcilePendingRevocations(context.Background()); err == nil {
		t.Fatal("ReconcilePendingRevocations() error = nil")
	}
	if got, err := secrets.Get(proofOwner, revocationProofCredential); err != nil || !bytes.Equal(got, proof) {
		t.Fatalf("proof after non-durable remote revoke = %x error=%v", got, err)
	}
}

func TestPendingRevocationFailureDoesNotBlockOtherCleanupTasks(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	pending := make(map[string]config.PendingRevocation)
	secrets := credentials.NewMemoryStore()
	for _, generation := range []string{"generation-a", "generation-b"} {
		owner := "owner-" + generation
		pending[generation] = config.PendingRevocation{
			Generation: generation, CleanupRequested: true, LocalDeviceID: generation,
			Device: config.Device{ClientDeviceID: generation, RevocationCredentialOwner: owner},
		}
		if err := secrets.Put(owner, revocationProofCredential, bytes.Repeat([]byte{3}, pairing.RevocationProofSize)); err != nil {
			t.Fatalf("seed proof for %s: %v", generation, err)
		}
	}
	if err := store.Save(config.Config{SchemaVersion: config.CurrentSchemaVersion, PendingRevocations: pending}); err != nil {
		t.Fatalf("seed pending revocations: %v", err)
	}
	transport := &runtimePairingTransport{}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport,
		RemovePinnedHost: func(_ string, alias string) error {
			if strings.HasSuffix(alias, "generation-a") {
				return errors.New("injected local cleanup failure")
			}
			return nil
		},
		RemoveSSHConfig: func(sshtransport.ManagedRoot, string) error { return nil },
	})
	if err := coordinator.ReconcilePendingRevocations(context.Background()); err == nil {
		t.Fatal("ReconcilePendingRevocations() error = nil")
	}
	if transport.revokeCalls != 2 || transport.revoked != "generation-b" {
		t.Fatalf("independent cleanup calls=%d last=%q, want both tasks attempted", transport.revokeCalls, transport.revoked)
	}
}

func TestLocalOnlyRemovalIsDurableInTheSlotReleaseTransaction(t *testing.T) {
	store := config.Store{Path: filepath.Join(t.TempDir(), "config.json")}
	locker := &trackingStateLocker{}
	device := config.Device{
		Name: "Windows", ClientDeviceID: "LOCAL-SYNC", PairingGeneration: "pairing-generation",
	}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		ActiveDevice:  "windows-peer",
		Devices:       map[string]config.Device{"windows-peer": device},
	}); err != nil {
		t.Fatalf("seed active device: %v", err)
	}
	coordinator := newMacPairingCoordinator(macPairingOptions{Store: store, ConfigTransactions: &configTransactions{locker: locker}})
	generation, err := coordinator.moveActiveToPending(context.Background(), "windows-peer", device, true)
	if err != nil {
		t.Fatalf("moveActiveToPending() error = %v", err)
	}
	cfg, err := store.Load()
	pending := cfg.PendingRevocations[generation]
	if err != nil || cfg.ActiveDevice != "" || !pending.RemoteRevoked {
		t.Fatalf("atomic local-only journal = %#v config=%#v error=%v", pending, cfg, err)
	}
	if locker.entries != 1 || locker.held {
		t.Fatalf("Docker context journal lock entries=%d held=%t", locker.entries, locker.held)
	}
}

func TestPendingRevocationUsesIndependentTimeoutForEveryTask(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	pending := make(map[string]config.PendingRevocation)
	for _, item := range []struct {
		generation string
		deviceID   string
	}{{"generation-a", "slow"}, {"generation-b", "fast"}} {
		owner := "owner-" + item.generation
		pending[item.generation] = config.PendingRevocation{
			Generation: item.generation, CleanupRequested: true, DockerRestored: true, LocalCleaned: true,
			Device: config.Device{
				ClientDeviceID: item.deviceID, PairingGeneration: item.generation, RevocationCredentialOwner: owner,
			},
		}
		if err := secrets.Put(owner, revocationProofCredential, bytes.Repeat([]byte{4}, pairing.RevocationProofSize)); err != nil {
			t.Fatalf("seed proof: %v", err)
		}
	}
	if err := store.Save(config.Config{SchemaVersion: config.CurrentSchemaVersion, PendingRevocations: pending}); err != nil {
		t.Fatalf("seed pending tasks: %v", err)
	}
	transport := &deadlineBlockingPairingTransport{runtimePairingTransport: &runtimePairingTransport{}}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, CleanupTaskTimeout: 20 * time.Millisecond,
	})
	if err := coordinator.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("ReconcilePendingRevocations() error = %v", err)
	}
	if !reflect.DeepEqual(transport.calls, []string{"slow", "fast"}) {
		t.Fatalf("revoke calls = %#v, want every task to receive a live context", transport.calls)
	}
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("load cleanup journal: %v", err)
	}
	if _, exists := cfg.PendingRevocations["generation-b"]; exists {
		t.Fatalf("fast task was starved: %#v", cfg.PendingRevocations)
	}
}

func TestLocalOnlyLeasePreventsRemoteRevokeAfterRestart(t *testing.T) {
	root := t.TempDir()
	store, secrets, generation := seedPendingRemoteCleanup(t, root)
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("load pending cleanup: %v", err)
	}
	pending := cfg.PendingRevocations[generation]
	pending.LocalCleaned = false
	cfg.PendingRevocations[generation] = pending
	if err := store.Save(cfg); err != nil {
		t.Fatalf("prepare local cleanup race: %v", err)
	}
	transport := &blockingRevokeTransport{}
	localStageStarted := make(chan struct{})
	releaseLocalStage := make(chan struct{})
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, ConfigTransactions: newConfigTransactions(store.Path), Secrets: secrets, Transport: transport,
		RemoveSSHConfig: func(sshtransport.ManagedRoot, string) error {
			close(localStageStarted)
			<-releaseLocalStage
			return nil
		},
	})
	localDone := make(chan error, 1)
	go func() {
		localDone <- coordinator.cleanupPendingRevocation(context.Background(), generation, true, dockercli.ContextChange{})
	}()
	waitForTestSignal(t, localStageStarted, "local-only cleanup stage")
	competing := newMacPairingCoordinator(macPairingOptions{
		Store: store, ConfigTransactions: newConfigTransactions(store.Path), Secrets: secrets, Transport: transport,
	})
	competingAttempted := make(chan struct{})
	competingDone := make(chan error, 1)
	go func() {
		close(competingAttempted)
		competingDone <- competing.cleanupPendingRevocation(context.Background(), generation, false, dockercli.ContextChange{})
	}()
	waitForTestSignal(t, competingAttempted, "competing remote cleanup attempt")
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("remote revoke calls while local-only owns generation = %d, want 0", got)
	}
	close(releaseLocalStage)
	if err := waitForTestError(t, localDone, "local-only cleanup completion"); err != nil {
		t.Fatalf("local-only cleanup: %v", err)
	}
	if err := waitForTestError(t, competingDone, "competing remote cleanup"); err != nil {
		t.Fatalf("competing remote cleanup: %v", err)
	}
	restarted := newMacPairingCoordinator(macPairingOptions{
		Store: store, ConfigTransactions: newConfigTransactions(store.Path), Secrets: secrets, Transport: transport,
	})
	if err := restarted.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("restart reconcile: %v", err)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("remote revoke calls after accepted local-only = %d, want 0", got)
	}
}

func TestRemoteRevokeLeaseWinsBeforeLocalOnly(t *testing.T) {
	root := t.TempDir()
	store, secrets, generation := seedPendingRemoteCleanup(t, root)
	transport := &blockingRevokeTransport{started: make(chan struct{}), release: make(chan struct{})}
	remote := newMacPairingCoordinator(macPairingOptions{
		Store: store, ConfigTransactions: newConfigTransactions(store.Path), Secrets: secrets, Transport: transport,
	})
	local := newMacPairingCoordinator(macPairingOptions{
		Store: store, ConfigTransactions: newConfigTransactions(store.Path), Secrets: secrets, Transport: transport,
	})
	remoteDone := make(chan error, 1)
	go func() {
		remoteDone <- remote.cleanupPendingRevocation(context.Background(), generation, false, dockercli.ContextChange{})
	}()
	waitForTestSignal(t, transport.started, "remote revoke start")
	err := local.cleanupPendingRevocation(context.Background(), generation, true, dockercli.ContextChange{})
	var publicErr *localapi.PublicError
	if !errors.As(err, &publicErr) || publicErr.Code != localapi.ErrorNeedsAction {
		t.Fatalf("local-only while remote lease is active error = %v, want clear needs-action state", err)
	}
	close(transport.release)
	if err := waitForTestError(t, remoteDone, "remote cleanup completion"); err != nil {
		t.Fatalf("remote cleanup: %v", err)
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("remote revoke calls = %d, want exactly 1", got)
	}
}

func TestTwoCleanupWorkersShareGenerationLease(t *testing.T) {
	root := t.TempDir()
	store, secrets, generation := seedPendingRemoteCleanup(t, root)
	transport := &blockingRevokeTransport{started: make(chan struct{}), release: make(chan struct{})}
	first := newMacPairingCoordinator(macPairingOptions{
		Store: store, ConfigTransactions: newConfigTransactions(store.Path), Secrets: secrets, Transport: transport,
	})
	second := newMacPairingCoordinator(macPairingOptions{
		Store: store, ConfigTransactions: newConfigTransactions(store.Path), Secrets: secrets, Transport: transport,
	})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.cleanupPendingRevocation(context.Background(), generation, false, dockercli.ContextChange{})
	}()
	waitForTestSignal(t, transport.started, "first remote revoke")
	if err := second.cleanupPendingRevocation(context.Background(), generation, false, dockercli.ContextChange{}); err != nil {
		t.Fatalf("second cleanup worker: %v", err)
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("concurrent remote revoke calls = %d, want 1", got)
	}
	close(transport.release)
	if err := waitForTestError(t, firstDone, "first cleanup worker"); err != nil {
		t.Fatalf("first cleanup worker: %v", err)
	}
}

func TestExpiredRemoteCleanupLeaseIsRecoverable(t *testing.T) {
	root := t.TempDir()
	store, secrets, generation := seedPendingRemoteCleanup(t, root)
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("load pending cleanup: %v", err)
	}
	pending := cfg.PendingRevocations[generation]
	pending.CleanupLeaseToken = "abandoned-owner"
	pending.CleanupLeaseExpiresAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	cfg.PendingRevocations[generation] = pending
	if err := store.Save(cfg); err != nil {
		t.Fatalf("seed abandoned lease: %v", err)
	}
	transport := &blockingRevokeTransport{}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, ConfigTransactions: newConfigTransactions(store.Path), Secrets: secrets, Transport: transport,
	})
	if err := coordinator.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("recover abandoned lease: %v", err)
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("remote revoke calls after abandoned lease = %d, want 1", got)
	}
}

func TestCleanupRenewsLeaseBeforeRemoteRevoke(t *testing.T) {
	root := t.TempDir()
	store, secrets, generation := seedPendingRemoteCleanup(t, root)
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("load pending cleanup: %v", err)
	}
	pending := cfg.PendingRevocations[generation]
	pending.LocalCleaned = false
	cfg.PendingRevocations[generation] = pending
	if err := store.Save(cfg); err != nil {
		t.Fatalf("prepare lease renewal: %v", err)
	}
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	transport := &blockingRevokeTransport{beforeRevoke: func() {
		cfg, err := store.Load()
		if err != nil {
			t.Fatalf("load renewed lease: %v", err)
		}
		expiresAt, err := time.Parse(time.RFC3339Nano, cfg.PendingRevocations[generation].CleanupLeaseExpiresAt)
		if err != nil || !expiresAt.After(now) {
			t.Fatalf("lease was not renewed before revoke: expires=%s now=%s error=%v", expiresAt, now, err)
		}
	}}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, ConfigTransactions: newConfigTransactions(store.Path), Secrets: secrets, Transport: transport,
		Now: func() time.Time { return now }, RemoteCleanupLease: time.Minute,
		RemoveSSHConfig: func(sshtransport.ManagedRoot, string) error {
			now = now.Add(2 * time.Minute)
			return nil
		},
	})
	if err := coordinator.cleanupPendingRevocation(context.Background(), generation, false, dockercli.ContextChange{}); err != nil {
		t.Fatalf("cleanup with renewed lease: %v", err)
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("remote revoke calls = %d, want 1", got)
	}
}

func TestRemoteTimeoutDoesNotCancelDockerRestoreOrLaterGeneration(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	host := "ssh://remote-docker-device-slow"
	pending := map[string]config.PendingRevocation{}
	for _, item := range []struct{ generation, deviceID string }{{"generation-a", "slow"}, {"generation-b", "fast"}} {
		owner := "owner-" + item.generation
		pending[item.generation] = config.PendingRevocation{
			Generation: item.generation, CleanupRequested: true, LocalCleaned: true,
			DockerContext: config.DockerContextChange{
				Name: "remote-docker", CurrentHost: host, OwnerToken: item.generation, Created: true,
			},
			Device: config.Device{ClientDeviceID: item.deviceID, PairingGeneration: item.generation, RevocationCredentialOwner: owner},
		}
		if item.deviceID == "fast" {
			entry := pending[item.generation]
			entry.DockerRestored = true
			entry.DockerContext = config.DockerContextChange{}
			pending[item.generation] = entry
		}
		if err := secrets.Put(owner, revocationProofCredential, bytes.Repeat([]byte{4}, pairing.RevocationProofSize)); err != nil {
			t.Fatalf("seed proof: %v", err)
		}
	}
	if err := store.Save(config.Config{SchemaVersion: config.CurrentSchemaVersion, PendingRevocations: pending}); err != nil {
		t.Fatalf("seed pending cleanup: %v", err)
	}
	docker := &contextObservingDockerExecutor{managedContextExecutor: managedContextExecutor{host: host, description: "Managed by Remote Docker; owner=generation-a"}}
	transport := &deadlineBlockingPairingTransport{runtimePairingTransport: &runtimePairingTransport{}}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, ConfigTransactions: newConfigTransactions(store.Path), Secrets: secrets, Transport: transport,
		Docker: docker, DockerCLI: "docker-real", RemoteCleanupTimeout: 20 * time.Millisecond,
		DockerCleanupTimeout: time.Second,
	})
	if err := coordinator.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("reconcile independent cleanup stages: %v", err)
	}
	if docker.contextErr != nil || !docker.restored {
		t.Fatalf("Docker restore context=%v restored=%t", docker.contextErr, docker.restored)
	}
	if !reflect.DeepEqual(transport.calls, []string{"slow", "fast"}) {
		t.Fatalf("remote calls = %#v, want slow timeout followed by fast generation", transport.calls)
	}
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("load cleanup journal: %v", err)
	}
	left, exists := cfg.PendingRevocations["generation-a"]
	if !exists || !left.DockerRestored || !left.LocalCleaned || left.RemoteRevoked {
		t.Fatalf("journal should retain only unfinished network stage: %#v", left)
	}
	if _, exists := cfg.PendingRevocations["generation-b"]; exists {
		t.Fatalf("later generation was not serviced: %#v", cfg.PendingRevocations)
	}
}

func TestParentCancellationStopsRemoteCleanup(t *testing.T) {
	root := t.TempDir()
	store, secrets, generation := seedPendingRemoteCleanup(t, root)
	transport := &blockingRevokeTransport{started: make(chan struct{})}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, ConfigTransactions: newConfigTransactions(store.Path), Secrets: secrets, Transport: transport,
		RemoteCleanupTimeout: time.Minute,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- coordinator.cleanupPendingRevocation(ctx, generation, false, dockercli.ContextChange{})
	}()
	waitForTestSignal(t, transport.started, "remote cleanup start")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("parent cancellation left cleanup goroutine blocked")
	}
}

func TestOldCleanupGenerationCannotDeleteNewPairingArtifacts(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	docker := &managedContextExecutor{}
	transport := &runtimePairingTransport{
		hostKey: testAuthorizedKey(t), revokeErrors: []error{errors.New("windows offline"), nil},
	}
	failLocalStageSave := false
	failedOnce := false
	options := macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: docker,
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:  filepath.Join(root, "ssh_config"), ManagedSSHRoot: testManagedSSHRoot(t, root),
		KnownHostsPath: filepath.Join(root, "known_hosts"), AgentSocketPath: filepath.Join(root, "ssh-agent.sock"),
		ControlDir: filepath.Join(root, "control"),
		SaveConfig: func(cfg config.Config) error {
			if failLocalStageSave && !failedOnce && cfg.ActiveDevice == "" {
				for _, pending := range cfg.PendingRevocations {
					if pending.CleanupRequested && pending.DockerContext.Name == "" {
						failedOnce = true
						return errors.New("injected local stage save failure")
					}
				}
			}
			return store.Save(cfg)
		},
	}
	coordinator := newMacPairingCoordinator(options)
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	first, err := coordinator.Status(context.Background(), started.SessionID)
	if err != nil || first.Device == nil {
		t.Fatalf("first Status() = %#v error=%v", first, err)
	}
	failLocalStageSave = true
	if err := coordinator.Unpair(context.Background(), first.Device.ID, false); err == nil {
		t.Fatal("Unpair() error = nil, want durable local-stage failure")
	}

	restarted := newMacPairingCoordinator(options)
	started, err = restarted.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	second, err := restarted.Status(context.Background(), started.SessionID)
	if err != nil || second.Device == nil {
		t.Fatalf("second Status() = %#v error=%v", second, err)
	}
	for _, credential := range []string{sshtransport.SSHPrivateKeyCredential, tunnel.IdentityCredential} {
		if _, err := secrets.Get(second.Device.ID, credential); err != nil {
			t.Fatalf("new pairing credential %q before retry: %v", credential, err)
		}
	}
	if err := restarted.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("ReconcilePendingRevocations() error = %v", err)
	}
	for _, credential := range []string{sshtransport.SSHPrivateKeyCredential, tunnel.IdentityCredential} {
		if _, err := secrets.Get(second.Device.ID, credential); err != nil {
			t.Fatalf("old cleanup deleted new pairing credential %q: %v", credential, err)
		}
	}
	wantHost := "ssh://remote-docker-device-" + second.Device.ID
	if docker.host != wantHost {
		t.Fatalf("old cleanup changed new Docker context host = %q, want %q", docker.host, wantHost)
	}
}

func TestRestartedPairingCancelCleansOldGenerationWithoutDeletingNewTrust(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	docker := &managedContextExecutor{
		host: "ssh://remote-docker-device-windows-peer", description: "Managed by Remote Docker; owner=new-generation",
	}
	oldGeneration := "old-generation"
	oldOwner := "pairing-generation-old"
	newOwner := "pairing-generation-new"
	serverPublicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate pairing server key: %v", err)
	}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion, ActiveDevice: "windows-peer",
		Devices: map[string]config.Device{"windows-peer": {
			Name: "Windows", Address: "192.168.1.20", PairingGeneration: "new-generation",
			RevocationCredentialOwner: newOwner,
			DockerContext: config.DockerContextChange{
				Name: "remote-docker", CurrentHost: "ssh://remote-docker-device-windows-peer",
				OwnerToken: "new-generation", Created: true,
			},
		}},
		PendingRevocations: map[string]config.PendingRevocation{oldGeneration: {
			Generation: oldGeneration, SessionID: "old-session", LocalDeviceID: "windows-peer",
			Device: config.Device{
				Name: "Windows", Address: "192.168.1.20", ClientDeviceID: "LOCAL-SYNC",
				PairingGeneration: oldGeneration, RevocationCredentialOwner: oldOwner,
				TunnelPeerPublicKey: tunnel.EncodePublicKey(serverPublicKey),
			},
			DockerContext: config.DockerContextChange{
				Name: "remote-docker", CurrentHost: "ssh://remote-docker-device-windows-peer",
				OwnerToken: oldGeneration, Created: true,
			},
		}},
	}); err != nil {
		t.Fatalf("seed old/new generations: %v", err)
	}
	if err := secrets.Put(oldOwner, revocationProofCredential, bytes.Repeat([]byte{1}, pairing.RevocationProofSize)); err != nil {
		t.Fatalf("seed old proof: %v", err)
	}
	if err := secrets.Put(newOwner, revocationProofCredential, bytes.Repeat([]byte{2}, pairing.RevocationProofSize)); err != nil {
		t.Fatalf("seed new proof: %v", err)
	}
	for _, credential := range []string{sshtransport.SSHPrivateKeyCredential, tunnel.IdentityCredential} {
		if err := secrets.Put("windows-peer", credential, []byte("new-trust")); err != nil {
			t.Fatalf("seed new credential %q: %v", credential, err)
		}
	}
	transport := &runtimePairingTransport{status: pairing.SessionCompleted}
	restarted := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: docker,
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		SSHConfigPath: filepath.Join(root, "ssh_config"), ManagedSSHRoot: testManagedSSHRoot(t, root),
		KnownHostsPath: filepath.Join(root, "known_hosts"),
	})

	result, err := restarted.CancelCurrent(context.Background(), "old-session")
	if err != nil || result.Status != string(pairing.SessionCancelled) {
		t.Fatalf("restarted CancelCurrent() = %#v error=%v", result, err)
	}
	cfg, err := store.Load()
	device, exists := cfg.Devices["windows-peer"]
	if err != nil || !exists || cfg.ActiveDevice != "windows-peer" || device.PairingGeneration != "new-generation" || len(cfg.PendingRevocations) != 0 {
		t.Fatalf("new trust after old cleanup = %#v error=%v", cfg, err)
	}
	if transport.revokeGeneration != oldGeneration {
		t.Fatalf("revoked generation = %q, want %q", transport.revokeGeneration, oldGeneration)
	}
	for _, command := range docker.Commands() {
		if len(command) >= 2 && (command[1] == "rm" || command[1] == "update") {
			t.Fatalf("old cancellation mutated new Docker context: %#v", docker.Commands())
		}
	}
	for _, credential := range []string{sshtransport.SSHPrivateKeyCredential, tunnel.IdentityCredential} {
		if _, err := secrets.Get("windows-peer", credential); err != nil {
			t.Fatalf("old cancellation deleted new credential %q: %v", credential, err)
		}
	}
	if _, err := secrets.Get(newOwner, revocationProofCredential); err != nil {
		t.Fatalf("old cancellation deleted new proof: %v", err)
	}
}

func TestPendingCompletionJournalIsNotRevokedUntilItsLeaseEnds(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	deviceID := "windows-peer"
	now := time.Now().UTC()
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		PendingRevocations: map[string]config.PendingRevocation{deviceID: {
			SessionID: "session-1", CompletionLeaseToken: "completion-owner",
			CompletionLeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano),
			Device:                   config.Device{ClientDeviceID: "LOCAL-SYNC"},
		}},
	}); err != nil {
		t.Fatalf("seed completion journal: %v", err)
	}
	secrets := credentials.NewMemoryStore()
	if err := secrets.Put(deviceID, revocationProofCredential, bytes.Repeat([]byte{1}, pairing.RevocationProofSize)); err != nil {
		t.Fatalf("seed revocation proof: %v", err)
	}
	transport := &runtimePairingTransport{}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport,
		Now:           func() time.Time { return now },
		SSHConfigPath: filepath.Join(root, "ssh_config"), ManagedSSHRoot: testManagedSSHRoot(t, root),
		KnownHostsPath: filepath.Join(root, "known_hosts"),
	})
	if err := coordinator.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("ReconcilePendingRevocations() with live lease error = %v", err)
	}
	if transport.revokeCalls != 0 {
		t.Fatalf("live completion was revoked: calls=%d", transport.revokeCalls)
	}
	now = now.Add(2 * time.Minute)
	if err := coordinator.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("ReconcilePendingRevocations() after restart error = %v", err)
	}
	if transport.revokeCalls != 1 {
		t.Fatalf("abandoned completion revoke calls=%d, want one", transport.revokeCalls)
	}
}

func TestDurableCompletionLeaseProtectsCommitFromSecondProcess(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	transport := &blockingConfirmPairingTransport{
		runtimePairingTransport: &runtimePairingTransport{hostKey: testAuthorizedKey(t)},
		started:                 make(chan struct{}), release: make(chan struct{}),
	}
	options := macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: &runtimeDockerExecutor{},
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:  filepath.Join(root, "ssh_config"), ManagedSSHRoot: testManagedSSHRoot(t, root),
		KnownHostsPath: filepath.Join(root, "known_hosts"), AgentSocketPath: filepath.Join(root, "ssh-agent.sock"),
		ControlDir: filepath.Join(root, "control"),
	}
	first := newMacPairingCoordinator(options)
	started, err := first.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	statusDone := make(chan error, 1)
	go func() {
		_, err := first.Status(context.Background(), started.SessionID)
		statusDone <- err
	}()
	waitForTestSignal(t, transport.started, "pairing confirmation")
	secondOptions := options
	secondOptions.ConfigTransactions = newConfigTransactions(store.Path)
	second := newMacPairingCoordinator(secondOptions)
	if err := second.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("second-process reconcile: %v", err)
	}
	if transport.revokeCalls != 0 {
		t.Fatalf("second process revoked live completion: calls=%d", transport.revokeCalls)
	}
	close(transport.release)
	if err := waitForTestError(t, statusDone, "pairing completion"); err != nil {
		t.Fatalf("pairing completion: %v", err)
	}
	cfg, err := store.Load()
	if err != nil || cfg.ActiveDevice == "" || len(cfg.PendingRevocations) != 0 {
		t.Fatalf("completed config = %#v error=%v", cfg, err)
	}
}

func TestExpiredCompletionLeaseCannotCommitLocalArtifacts(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	now := time.Now().UTC()
	generation := "expired-completion"
	token := "expired-owner"
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		PendingRevocations: map[string]config.PendingRevocation{generation: {
			Generation: generation, CompletionLeaseToken: token,
			CompletionLeaseExpiresAt: now.Add(-time.Second).Format(time.RFC3339Nano),
		}},
	}); err != nil {
		t.Fatalf("seed expired completion lease: %v", err)
	}
	docker := &runtimeDockerExecutor{}
	knownHostsPath := filepath.Join(root, "known_hosts")
	sshConfigPath := filepath.Join(root, "ssh_config")
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: credentials.NewMemoryStore(), Docker: docker,
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		KnownHostsPath: knownHostsPath, SSHConfigPath: sshConfigPath,
		AgentSocketPath: filepath.Join(root, "ssh-agent.sock"), ControlDir: filepath.Join(root, "control"),
		Now: func() time.Time { return now },
	})
	_, _, err := coordinator.commitPairingArtifacts(
		context.Background(),
		pendingPairing{cleanupID: generation, completionLeaseToken: token},
		"peer", pairing.DeviceRecord{SSHHostPublicKey: testAuthorizedKey(t)}, config.Device{},
	)
	var publicErr *localapi.PublicError
	if !errors.As(err, &publicErr) || publicErr.Code != localapi.ErrorNeedsAction {
		t.Fatalf("expired completion commit error = %v, want needs-action", err)
	}
	for _, path := range []string{knownHostsPath, sshConfigPath} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("expired completion wrote %s: %v", path, statErr)
		}
	}
	if commands := docker.calls; len(commands) != 0 {
		t.Fatalf("expired completion mutated Docker: %#v", commands)
	}
}

func TestMacPairingReportsProtocolUpgradeGate(t *testing.T) {
	transport := &runtimePairingTransport{bootstrapErr: pairing.ErrProtocolUpgradeRequired}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store:   config.Store{Path: filepath.Join(t.TempDir(), "config.json")},
		Secrets: credentials.NewMemoryStore(), Transport: transport, Docker: &runtimeDockerExecutor{},
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
	})
	_, err := coordinator.Start(context.Background(), "windows-peer")
	var publicErr *localapi.PublicError
	if !errors.As(err, &publicErr) || publicErr.Code != localapi.ErrorNeedsAction ||
		!strings.Contains(strings.ToLower(publicErr.Message), "update remote docker") {
		t.Fatalf("protocol mismatch error = %#v, want clear update gate", err)
	}
}

func TestMacPairingDiscoveryReportsProtocolUpgradeGate(t *testing.T) {
	transport := discoveryPairingTransport{
		discover: func(context.Context) ([]discovery.Peer, error) {
			return []discovery.Peer{{
				InstanceID: "old-windows", DeviceID: "old-windows", Pairing: true, Port: tunnel.TunnelPort,
				Addresses: []net.IP{net.ParseIP("192.168.1.20")},
			}}, nil
		},
		inspect: func(context.Context, string, string) (pairing.Info, error) {
			return pairing.Info{}, pairing.ErrProtocolUpgradeRequired
		},
	}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: config.Store{Path: filepath.Join(t.TempDir(), "config.json")}, Transport: transport,
	})
	_, err := coordinator.Candidates(context.Background())
	var publicErr *localapi.PublicError
	if !errors.As(err, &publicErr) || publicErr.Code != localapi.ErrorNeedsAction ||
		!strings.Contains(strings.ToLower(publicErr.Message), "update remote docker") {
		t.Fatalf("protocol discovery mismatch error = %#v, want clear update gate", err)
	}
}

func TestMacPairingDiscoveryKeepsCompatiblePeerBesideOldPeer(t *testing.T) {
	transport := discoveryPairingTransport{
		discover: func(context.Context) ([]discovery.Peer, error) {
			return []discovery.Peer{
				{InstanceID: "old-windows", DeviceID: "old-windows", Pairing: true, Port: tunnel.TunnelPort, Addresses: []net.IP{net.ParseIP("192.168.1.20")}},
				{InstanceID: "new-windows", DeviceID: "new-windows", Pairing: true, Port: tunnel.TunnelPort, Addresses: []net.IP{net.ParseIP("192.168.1.21")}},
			}, nil
		},
		inspect: func(_ context.Context, endpoint, instanceID string) (pairing.Info, error) {
			if instanceID == "old-windows" {
				return pairing.Info{}, pairing.ErrProtocolUpgradeRequired
			}
			return pairing.Info{InstanceID: instanceID, DisplayName: "Current Windows"}, nil
		},
	}
	targets, err := transport.Candidates(context.Background())
	if err != nil || len(targets) != 1 || targets[0].InstanceID != "new-windows" {
		t.Fatalf("mixed-version discovery targets=%#v error=%v", targets, err)
	}
}

func TestExpiredDurableCompletionLeaseResumesCleanupAfterCrash(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	now := time.Now().UTC()
	generation := "generation-crashed"
	owner := "owner-crashed"
	serverPublicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate pairing server key: %v", err)
	}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		PendingRevocations: map[string]config.PendingRevocation{generation: {
			Generation: generation, SessionID: "session-crashed",
			CompletionLeaseToken:     "dead-process",
			CompletionLeaseExpiresAt: now.Add(-time.Second).Format(time.RFC3339Nano),
			DockerRestored:           true, LocalCleaned: true,
			Device: config.Device{
				Address: "192.168.1.20", ClientDeviceID: "LOCAL-SYNC",
				TunnelPeerPublicKey: tunnel.EncodePublicKey(serverPublicKey),
				PairingGeneration:   generation, RevocationCredentialOwner: owner,
			},
		}},
	}); err != nil {
		t.Fatalf("seed crashed completion: %v", err)
	}
	if err := secrets.Put(owner, revocationProofCredential, bytes.Repeat([]byte{5}, pairing.RevocationProofSize)); err != nil {
		t.Fatalf("seed proof: %v", err)
	}
	transport := &runtimePairingTransport{status: pairing.SessionExpired}
	restarted := newMacPairingCoordinator(macPairingOptions{
		Store: store, ConfigTransactions: newConfigTransactions(store.Path), Secrets: secrets, Transport: transport,
		Now: func() time.Time { return now },
	})
	if err := restarted.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("recover expired completion lease: %v", err)
	}
	if transport.revokeCalls != 2 {
		t.Fatalf("remote revoke calls after crash = %d, want pre-fence and post-fence revoke", transport.revokeCalls)
	}
	cfg, err := store.Load()
	if err != nil || len(cfg.PendingRevocations) != 0 {
		t.Fatalf("cleanup after expired completion lease = %#v error=%v", cfg.PendingRevocations, err)
	}
}

func TestMacPairingPublishesRollbackJournalWithLiveLease(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	proofOwner := "pairing-session-session-1"
	now := time.Now().UTC()
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		PendingRevocations: map[string]config.PendingRevocation{"generation-1": {
			SessionID: "session-1", CompletionLeaseToken: "starting-owner",
			CompletionLeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano), Device: config.Device{
				ClientDeviceID: "LOCAL-SYNC", RevocationCredentialOwner: proofOwner,
			},
		}},
	}); err != nil {
		t.Fatalf("seed rollback journal: %v", err)
	}
	secrets := credentials.NewMemoryStore()
	if err := secrets.Put(proofOwner, revocationProofCredential, bytes.Repeat([]byte{1}, pairing.RevocationProofSize)); err != nil {
		t.Fatalf("seed rollback proof: %v", err)
	}
	transport := &runtimePairingTransport{}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport,
		Docker:         &runtimeDockerExecutor{},
		Now:            func() time.Time { return now },
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
	})
	if err := coordinator.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("ReconcilePendingRevocations() error = %v", err)
	}
	if transport.revokeCalls != 0 {
		t.Fatalf("journal published by active Start was revoked: calls=%d", transport.revokeCalls)
	}
	cfg, err := store.Load()
	if err != nil || len(cfg.PendingRevocations) != 1 {
		t.Fatalf("live rollback journal = %#v error=%v", cfg.PendingRevocations, err)
	}
	for _, pending := range cfg.PendingRevocations {
		if pending.CleanupRequested {
			t.Fatal("live rollback journal was marked for cleanup")
		}
	}
}

func TestMacPairingRemovalRestoresDockerContextAndAllowsRepairing(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	docker := &managedContextExecutor{}
	transport := &runtimePairingTransport{hostKey: testAuthorizedKey(t)}
	options := macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: docker,
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:  filepath.Join(root, "ssh_config"), ManagedSSHRoot: testManagedSSHRoot(t, root),
		KnownHostsPath: filepath.Join(root, "known_hosts"), AgentSocketPath: filepath.Join(root, "ssh-agent.sock"),
		ControlDir: filepath.Join(root, "control"),
	}
	coordinator := newMacPairingCoordinator(options)
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	first, err := coordinator.Status(context.Background(), started.SessionID)
	if err != nil || first.Device == nil {
		t.Fatalf("first pairing status=%#v error=%v", first, err)
	}
	if err := coordinator.Unpair(context.Background(), first.Device.ID, false); err != nil {
		t.Fatalf("Unpair() error = %v", err)
	}
	if docker.host != "" {
		t.Fatalf("Docker context host after removal = %q, want removed", docker.host)
	}
	transport.hostKey = testAuthorizedKey(t)
	restarted := newMacPairingCoordinator(options)
	started, err = restarted.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	second, err := restarted.Status(context.Background(), started.SessionID)
	if err != nil || second.Device == nil {
		t.Fatalf("second pairing status=%#v error=%v", second, err)
	}
	if docker.host != "ssh://remote-docker-device-"+second.Device.ID {
		t.Fatalf("Docker context host after re-pair = %q", docker.host)
	}
}

func TestProductionPairStatusObserveOnlyNeverCompletesReplacement(t *testing.T) {
	tests := []struct {
		name   string
		status pairing.SessionState
		record *pairing.DeviceRecord
	}{
		{name: "approved", status: pairing.SessionApproved},
		{name: "completed", status: pairing.SessionCompleted, record: &pairing.DeviceRecord{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			transport := &runtimePairingTransport{status: tt.status}
			coordinator := newMacPairingCoordinator(macPairingOptions{
				Store:          config.Store{Path: filepath.Join(root, "config.json")},
				Secrets:        credentials.NewMemoryStore(),
				Transport:      transport,
				Docker:         &runtimeDockerExecutor{},
				ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
			})
			started, err := coordinator.Start(context.Background(), "windows-peer")
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			coordinator.mu.Lock()
			coordinator.pending.record = tt.record
			secret := coordinator.pending.privateKeyPEM
			coordinator.mu.Unlock()
			afterPair := make(chan struct{}, 1)
			controller := &productionAgentController{
				pairing:   coordinator,
				afterPair: func(context.Context) { afterPair <- struct{}{} },
			}
			raw, _ := json.Marshal(localapi.PairSessionParams{SessionID: started.SessionID, ObserveOnly: true})

			result, err := controller.Handle(context.Background(), localapi.MethodPairStatus, raw)
			if err != nil {
				t.Fatalf("observe-only PairStatus error = %v", err)
			}
			status := result.(localapi.PairingStatusResult)
			if status.Status != string(tt.status) || status.Device != nil {
				t.Fatalf("observe-only status = %#v", status)
			}
			coordinator.mu.Lock()
			pending := coordinator.pending
			coordinator.mu.Unlock()
			if pending == nil || pending.descriptor.ID != started.SessionID || pending.completing || allZero(secret) {
				t.Fatalf("observe-only status mutated pending pairing = %#v", pending)
			}
			select {
			case <-afterPair:
				t.Fatal("observe-only completed replacement started post-pair recovery")
			default:
			}
		})
	}
}

func TestProductionPairStatusObserveOnlyClearsTerminalReplacementAndShutdownAbandonsSecret(t *testing.T) {
	for _, state := range []pairing.SessionState{pairing.SessionRejected, pairing.SessionExpired} {
		t.Run(string(state), func(t *testing.T) {
			root := t.TempDir()
			transport := &runtimePairingTransport{status: state}
			coordinator := newMacPairingCoordinator(macPairingOptions{
				Store:          config.Store{Path: filepath.Join(root, "config.json")},
				Secrets:        credentials.NewMemoryStore(),
				Transport:      transport,
				Docker:         &runtimeDockerExecutor{},
				ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
			})
			started, err := coordinator.Start(context.Background(), "windows-peer")
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			coordinator.mu.Lock()
			secret := coordinator.pending.privateKeyPEM
			coordinator.mu.Unlock()
			controller := &productionAgentController{pairing: coordinator}
			raw, _ := json.Marshal(localapi.PairSessionParams{SessionID: started.SessionID, ObserveOnly: true})

			if _, err := controller.Handle(context.Background(), localapi.MethodPairStatus, raw); err != nil {
				t.Fatalf("terminal observe-only PairStatus error = %v", err)
			}
			coordinator.mu.Lock()
			pending := coordinator.pending
			coordinator.mu.Unlock()
			if pending != nil || !allZero(secret) {
				t.Fatalf("terminal observation retained pending secret: pending=%#v zero=%t", pending, allZero(secret))
			}
		})
	}

	root := t.TempDir()
	transport := &runtimePairingTransport{cancelErr: errors.New("remote cancellation unavailable")}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store:          config.Store{Path: filepath.Join(root, "config.json")},
		Secrets:        credentials.NewMemoryStore(),
		Transport:      transport,
		Docker:         &runtimeDockerExecutor{},
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
	})
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	coordinator.mu.Lock()
	secret := coordinator.pending.privateKeyPEM
	coordinator.mu.Unlock()
	agent := NewAgent(nil, nil, &productionAgentController{pairing: coordinator})
	agent.abandonPairing(started.SessionID)
	coordinator.mu.Lock()
	pending := coordinator.pending
	coordinator.mu.Unlock()
	if pending != nil || !allZero(secret) {
		t.Fatalf("shutdown abandon retained pending secret: pending=%#v zero=%t", pending, allZero(secret))
	}
}

func TestMacPairingLocalForgetSkipsRemoteRevokeAndPreservesWorkspaces(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	deviceID := "trusted-windows"
	device := config.Device{
		Name: "Saved Windows", Address: "192.168.1.20", SSHPort: 49222,
		SSHHostPublicKey: testAuthorizedKey(t), SyncthingDeviceID: "WINDOWS-SYNC", ClientDeviceID: "LOCAL-SYNC",
	}
	workspaces := map[string]config.Workspace{
		"workspace-1": {Path: "/Users/developer/project"},
	}
	localIdentity := []byte("local-syncthing-identity")
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion, ActiveDevice: deviceID,
		LocalSyncthingDeviceID: "LOCAL-SYNC", LocalSyncthingIdentity: append([]byte(nil), localIdentity...),
		Devices: map[string]config.Device{deviceID: device}, Workspaces: workspaces,
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	secrets := credentials.NewMemoryStore()
	if err := secrets.Put(deviceID, sshtransport.SSHPrivateKeyCredential, []byte("managed-private-key")); err != nil {
		t.Fatalf("seed SSH identity: %v", err)
	}
	sshConfigPath := filepath.Join(root, "ssh_config")
	knownHostsPath := filepath.Join(root, "known_hosts")
	if err := os.WriteFile(sshConfigPath, []byte("managed SSH config\n"), 0o600); err != nil {
		t.Fatalf("seed managed SSH config: %v", err)
	}
	managedAlias := "remote-docker-device-" + deviceID
	unrelatedKnownHost := "unrelated.example ssh-ed25519 unrelated-key\n"
	if err := os.WriteFile(knownHostsPath, []byte(managedAlias+" "+device.SSHHostPublicKey+"\n"+unrelatedKnownHost), 0o600); err != nil {
		t.Fatalf("seed known_hosts: %v", err)
	}
	transport := &runtimePairingTransport{revokeErr: errors.New("peer unreachable")}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport,
		SSHConfigPath: sshConfigPath, ManagedSSHRoot: testManagedSSHRoot(t, root), KnownHostsPath: knownHostsPath,
	})

	if err := coordinator.Unpair(context.Background(), deviceID, true); err != nil {
		t.Fatalf("Unpair(local only) error = %v", err)
	}
	if transport.revokeCalls != 0 {
		t.Fatalf("remote revoke calls = %d, want zero", transport.revokeCalls)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("load config after local forget: %v", err)
	}
	if got.ActiveDevice != "" || len(got.Devices) != 0 {
		t.Fatalf("trusted device survived local forget: %#v", got)
	}
	if !reflect.DeepEqual(got.Workspaces, workspaces) {
		t.Fatalf("workspaces changed: got %#v want %#v", got.Workspaces, workspaces)
	}
	if got.LocalSyncthingDeviceID != "LOCAL-SYNC" || !bytes.Equal(got.LocalSyncthingIdentity, localIdentity) {
		t.Fatalf("local Syncthing identity changed: id=%q identity=%q", got.LocalSyncthingDeviceID, got.LocalSyncthingIdentity)
	}
	if _, err := secrets.Get(deviceID, sshtransport.SSHPrivateKeyCredential); !errors.Is(err, credentials.ErrNotFound) {
		t.Fatalf("private key after local forget error = %v", err)
	}
	if _, err := os.Stat(sshConfigPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed SSH config after local forget error = %v", err)
	}
	knownHosts, err := os.ReadFile(knownHostsPath)
	if err != nil || string(knownHosts) != unrelatedKnownHost {
		t.Fatalf("known_hosts after local forget = %q, error = %v", knownHosts, err)
	}
}

func TestMacPairingLocalForgetRetriesEveryPartialCleanupFailure(t *testing.T) {
	tests := []struct {
		name              string
		knownHostsPresent bool
		sshConfigPresent  bool
		credentialPresent bool
		inject            func(*localForgetFixture)
	}{
		{
			name: "known_hosts removal", knownHostsPresent: true, sshConfigPresent: true, credentialPresent: true,
			inject: func(fixture *localForgetFixture) {
				failed := false
				fixture.options.RemovePinnedHost = func(path, alias string) error {
					if !failed {
						failed = true
						return errors.New("injected known_hosts failure")
					}
					return sshtransport.RemovePinnedHost(path, alias)
				}
			},
		},
		{
			name: "ssh_config removal after known_hosts", sshConfigPresent: true, credentialPresent: true,
			inject: func(fixture *localForgetFixture) {
				failed := false
				fixture.options.RemoveSSHConfig = func(root sshtransport.ManagedRoot, path string) error {
					if !failed {
						failed = true
						return errors.New("injected ssh_config failure")
					}
					return root.RemoveConfig(path)
				}
			},
		},
		{
			name: "keyring deletion after ssh_config", credentialPresent: true,
			inject: func(fixture *localForgetFixture) {
				fixture.secrets.deleteFailures = 1
			},
		},
		{
			name: "config save after keyring deletion",
			inject: func(fixture *localForgetFixture) {
				failed := false
				fixture.options.SaveConfig = func(cfg config.Config) error {
					if !failed {
						for _, pending := range cfg.PendingRevocations {
							if pending.LocalCleaned {
								failed = true
								return errors.New("injected config save failure")
							}
						}
					}
					return fixture.store.Save(cfg)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newLocalForgetFixture(t)
			tt.inject(fixture)
			coordinator := newMacPairingCoordinator(fixture.options)

			if err := coordinator.Unpair(context.Background(), fixture.deviceID, true); err == nil {
				t.Fatal("first Unpair(local only) error = nil")
			}
			failedConfig, err := fixture.store.Load()
			if err != nil {
				t.Fatalf("load config after injected failure: %v", err)
			}
			if failedConfig.ActiveDevice != "" || len(failedConfig.Devices) != 0 || len(failedConfig.PendingRevocations) != 1 {
				t.Fatalf("retry journal missing after injected failure = %#v", failedConfig)
			}
			fixture.assertPartialCleanup(t, tt.knownHostsPresent, tt.sshConfigPresent, tt.credentialPresent)

			if err := coordinator.Unpair(context.Background(), fixture.deviceID, true); err != nil {
				t.Fatalf("Unpair(local only) retry error = %v", err)
			}
			fixture.assertForgotten(t)
		})
	}
}

func (f *localForgetFixture) assertPartialCleanup(t *testing.T, knownHostsPresent, sshConfigPresent, credentialPresent bool) {
	t.Helper()
	knownHosts, err := os.ReadFile(filepath.Join(f.root, "known_hosts"))
	if err != nil {
		t.Fatalf("read known_hosts after failure: %v", err)
	}
	if got := strings.Contains(string(knownHosts), "remote-docker-device-"+f.deviceID+" "); got != knownHostsPresent {
		t.Fatalf("managed known-host presence = %t, want %t: %q", got, knownHostsPresent, knownHosts)
	}
	_, sshConfigErr := os.Stat(filepath.Join(f.root, "ssh_config"))
	if sshConfigErr != nil && !errors.Is(sshConfigErr, os.ErrNotExist) {
		t.Fatalf("stat managed ssh_config after failure: %v", sshConfigErr)
	}
	if got := sshConfigErr == nil; got != sshConfigPresent {
		t.Fatalf("managed ssh_config presence = %t, want %t, error = %v", got, sshConfigPresent, sshConfigErr)
	}
	_, credentialErr := f.secrets.Get(f.deviceID, sshtransport.SSHPrivateKeyCredential)
	if credentialErr != nil && !errors.Is(credentialErr, credentials.ErrNotFound) {
		t.Fatalf("read managed credential after failure: %v", credentialErr)
	}
	if got := credentialErr == nil; got != credentialPresent {
		t.Fatalf("managed credential presence = %t, want %t, error = %v", got, credentialPresent, credentialErr)
	}
}

func TestMacPairingNormalRetryPersistsRemoteRevokeBeforeRetryingLocalCleanup(t *testing.T) {
	fixture := newLocalForgetFixture(t)
	fixture.transport.revokeErrors = []error{nil}
	failed := false
	fixture.options.RemoveSSHConfig = func(root sshtransport.ManagedRoot, path string) error {
		if !failed {
			failed = true
			return errors.New("injected failure after remote revoke")
		}
		return root.RemoveConfig(path)
	}
	coordinator := newMacPairingCoordinator(fixture.options)

	if err := coordinator.Unpair(context.Background(), fixture.deviceID, false); err == nil {
		t.Fatal("first normal Unpair error = nil")
	}
	if err := coordinator.Unpair(context.Background(), fixture.deviceID, false); err != nil {
		t.Fatalf("second normal Unpair error = %v", err)
	}
	if fixture.transport.revokeCalls != 1 {
		t.Fatalf("durable remote revoke calls = %d, want one", fixture.transport.revokeCalls)
	}
	cfg, err := fixture.store.Load()
	if err != nil || len(cfg.PendingRevocations) != 0 {
		t.Fatalf("completed retry journal = %#v error=%v", cfg.PendingRevocations, err)
	}
	fixture.assertForgotten(t)
}

func TestMacPairingLegacyDeviceRequiresExplicitLocalOnlyForget(t *testing.T) {
	fixture := newLocalForgetFixture(t)
	cfg, err := fixture.store.Load()
	if err != nil {
		t.Fatalf("load legacy config: %v", err)
	}
	legacy := cfg.Devices[fixture.deviceID]
	legacy.ClientDeviceID = ""
	cfg.Devices[fixture.deviceID] = legacy
	if err := fixture.store.Save(cfg); err != nil {
		t.Fatalf("save legacy config: %v", err)
	}
	coordinator := newMacPairingCoordinator(fixture.options)

	err = coordinator.Unpair(context.Background(), fixture.deviceID, false)
	var public *localapi.PublicError
	if !errors.As(err, &public) || public.Code != localapi.ErrorRemoteRevokeUnavailable {
		t.Fatalf("legacy normal Unpair error = %v, want typed remote revoke unavailable", err)
	}
	if fixture.transport.revokeCalls != 0 {
		t.Fatalf("legacy normal Unpair attempted remote revoke: calls=%d", fixture.transport.revokeCalls)
	}
	fixture.assertPartialCleanup(t, true, true, true)
	unchanged, err := fixture.store.Load()
	if err != nil || unchanged.ActiveDevice != fixture.deviceID || len(unchanged.Devices) != 1 || len(unchanged.Workspaces) != 1 {
		t.Fatalf("legacy trust changed before local-only confirmation: cfg=%#v error=%v", unchanged, err)
	}

	if err := coordinator.Unpair(context.Background(), fixture.deviceID, true); err != nil {
		t.Fatalf("legacy local-only Unpair error = %v", err)
	}
	fixture.assertForgotten(t)
}

func TestSharedConfigTransactionPreservesWorkspaceAddDuringForget(t *testing.T) {
	fixture := newLocalForgetFixture(t)
	transactions := &configTransactions{}
	fixture.options.ConfigTransactions = transactions
	forgetSaving := make(chan struct{})
	releaseForgetSave := make(chan struct{})
	fixture.options.SaveConfig = func(cfg config.Config) error {
		select {
		case <-forgetSaving:
		default:
			close(forgetSaving)
			<-releaseForgetSave
		}
		return fixture.store.Save(cfg)
	}
	coordinator := newMacPairingCoordinator(fixture.options)
	workspacePath := t.TempDir()
	workspaceAttempted := make(chan struct{})
	controller := &productionAgentController{
		store:                   fixture.store,
		configTransactions:      transactions,
		beforeConfigTransaction: func() { close(workspaceAttempted) },
	}
	forgetDone := make(chan error, 1)
	go func() { forgetDone <- coordinator.Unpair(context.Background(), fixture.deviceID, true) }()
	waitForTestSignal(t, forgetSaving, "forget config save")
	workspaceDone := make(chan error, 1)
	go func() {
		_, err := controller.addWorkspace(workspacePath)
		workspaceDone <- err
	}()
	waitForTestSignal(t, workspaceAttempted, "workspace transaction attempt")
	select {
	case err := <-workspaceDone:
		t.Fatalf("workspace add bypassed forget transaction: %v", err)
	default:
	}
	close(releaseForgetSave)
	if err := waitForTestError(t, forgetDone, "forget completion"); err != nil {
		t.Fatalf("forget error = %v", err)
	}
	if err := waitForTestError(t, workspaceDone, "workspace add completion"); err != nil {
		t.Fatalf("workspace add error = %v", err)
	}
	cfg, err := fixture.store.Load()
	if err != nil {
		t.Fatalf("load final config: %v", err)
	}
	if cfg.ActiveDevice != "" || len(cfg.Devices) != 0 || len(cfg.Workspaces) != 2 {
		t.Fatalf("final config lost forget or workspace add = %#v", cfg)
	}
}

func TestSharedConfigTransactionPreservesWorkspaceRemoveBeforeForget(t *testing.T) {
	fixture := newLocalForgetFixture(t)
	transactions := &configTransactions{}
	fixture.options.ConfigTransactions = transactions
	forgetAttempted := make(chan struct{})
	fixture.options.BeforeConfigTransaction = func() { close(forgetAttempted) }
	coordinator := newMacPairingCoordinator(fixture.options)
	workspaceSaving := make(chan struct{})
	releaseWorkspaceSave := make(chan struct{})
	controller := &productionAgentController{
		store:              fixture.store,
		configTransactions: transactions,
		beforeConfigSave: func() {
			close(workspaceSaving)
			<-releaseWorkspaceSave
		},
	}
	workspaceDone := make(chan error, 1)
	go func() {
		_, err := controller.removeWorkspace("workspace")
		workspaceDone <- err
	}()
	waitForTestSignal(t, workspaceSaving, "workspace config save")
	forgetDone := make(chan error, 1)
	go func() { forgetDone <- coordinator.Unpair(context.Background(), fixture.deviceID, true) }()
	waitForTestSignal(t, forgetAttempted, "forget transaction attempt")
	select {
	case err := <-forgetDone:
		t.Fatalf("forget bypassed workspace transaction: %v", err)
	default:
	}
	close(releaseWorkspaceSave)
	if err := waitForTestError(t, workspaceDone, "workspace remove completion"); err != nil {
		t.Fatalf("workspace remove error = %v", err)
	}
	if err := waitForTestError(t, forgetDone, "forget completion"); err != nil {
		t.Fatalf("forget error = %v", err)
	}
	cfg, err := fixture.store.Load()
	if err != nil {
		t.Fatalf("load final config: %v", err)
	}
	if cfg.ActiveDevice != "" || len(cfg.Devices) != 0 || len(cfg.Workspaces) != 0 {
		t.Fatalf("final config resurrected trust or workspace = %#v", cfg)
	}
}

func TestStateConfigTransactionSerializesIndependentOwners(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{SchemaVersion: config.CurrentSchemaVersion}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	journalOwner := newConfigTransactions(store.Path)
	workspaceOwner := newConfigTransactions(store.Path)
	workspacePath := t.TempDir()
	workspaceLoaded := make(chan struct{})
	releaseWorkspace := make(chan struct{})
	controller := &productionAgentController{
		store: store, configTransactions: workspaceOwner,
		beforeConfigSave: func() {
			close(workspaceLoaded)
			<-releaseWorkspace
		},
	}
	workspaceDone := make(chan error, 1)
	go func() {
		_, err := controller.addWorkspace(workspacePath)
		workspaceDone <- err
	}()
	waitForTestSignal(t, workspaceLoaded, "workspace stale read")
	journalAttempted := make(chan struct{})
	journalDone := make(chan error, 1)
	go func() {
		close(journalAttempted)
		journalDone <- journalOwner.Run(func() error {
			cfg, err := loadAgentConfig(store)
			if err != nil {
				return err
			}
			cfg.PendingRevocations = map[string]config.PendingRevocation{"generation-1": {
				Generation: "generation-1", CleanupRequested: true, RemoteRevoked: true,
				DockerRestored: true, LocalCleaned: true,
			}}
			return store.Save(cfg)
		})
	}()
	waitForTestSignal(t, journalAttempted, "journal transaction attempt")
	select {
	case err := <-journalDone:
		t.Fatalf("independent journal owner bypassed state transaction: %v", err)
	default:
	}
	close(releaseWorkspace)
	if err := waitForTestError(t, workspaceDone, "workspace owner completion"); err != nil {
		t.Fatalf("workspace transaction: %v", err)
	}
	if err := waitForTestError(t, journalDone, "journal owner completion"); err != nil {
		t.Fatalf("journal transaction: %v", err)
	}
	cfg, err := store.Load()
	if err != nil || len(cfg.Workspaces) != 1 || len(cfg.PendingRevocations) != 1 {
		t.Fatalf("cross-process state transaction lost data: cfg=%#v error=%v", cfg, err)
	}
	restarted := newMacPairingCoordinator(macPairingOptions{
		Store: store, ConfigTransactions: newConfigTransactions(store.Path), Secrets: credentials.NewMemoryStore(),
	})
	if err := restarted.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("restart could not recover durable journal: %v", err)
	}
}

type localForgetFixture struct {
	root      string
	deviceID  string
	store     config.Store
	secrets   *failingDeleteCredentialStore
	transport *runtimePairingTransport
	options   macPairingOptions
}

func newLocalForgetFixture(t *testing.T) *localForgetFixture {
	t.Helper()
	root := t.TempDir()
	deviceID := "trusted-windows"
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion, ActiveDevice: deviceID,
		LocalSyncthingDeviceID: "LOCAL-SYNC", LocalSyncthingIdentity: []byte("local-identity"),
		Devices: map[string]config.Device{deviceID: {
			Name: "Saved Windows", Address: "192.168.1.20", SSHPort: 49222,
			ClientDeviceID: "LOCAL-SYNC", SSHHostPublicKey: testAuthorizedKey(t), SyncthingDeviceID: "WINDOWS-SYNC",
		}},
		Workspaces: map[string]config.Workspace{"workspace": {Path: "/Users/developer/project"}},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	memory := credentials.NewMemoryStore()
	if err := memory.Put(deviceID, sshtransport.SSHPrivateKeyCredential, []byte("private-key")); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if err := memory.Put(deviceID, revocationProofCredential, bytes.Repeat([]byte{1}, pairing.RevocationProofSize)); err != nil {
		t.Fatalf("seed revocation proof: %v", err)
	}
	secrets := &failingDeleteCredentialStore{Store: memory}
	sshConfigPath := filepath.Join(root, "ssh_config")
	if err := os.WriteFile(sshConfigPath, []byte("managed config\n"), 0o600); err != nil {
		t.Fatalf("seed ssh_config: %v", err)
	}
	knownHostsPath := filepath.Join(root, "known_hosts")
	if err := os.WriteFile(knownHostsPath, []byte("remote-docker-device-"+deviceID+" ssh-ed25519 managed-key\nunrelated ssh-ed25519 unrelated-key\n"), 0o600); err != nil {
		t.Fatalf("seed known_hosts: %v", err)
	}
	transport := &runtimePairingTransport{}
	return &localForgetFixture{
		root: root, deviceID: deviceID, store: store, secrets: secrets, transport: transport,
		options: macPairingOptions{
			Store: store, Secrets: secrets, Transport: transport,
			SSHConfigPath: sshConfigPath, ManagedSSHRoot: testManagedSSHRoot(t, root), KnownHostsPath: knownHostsPath,
		},
	}
}

func (f *localForgetFixture) assertForgotten(t *testing.T) {
	t.Helper()
	cfg, err := f.store.Load()
	if err != nil {
		t.Fatalf("load config after retry: %v", err)
	}
	if cfg.ActiveDevice != "" || len(cfg.Devices) != 0 || len(cfg.PendingRevocations) != 0 || len(cfg.Workspaces) != 1 ||
		cfg.LocalSyncthingDeviceID != "LOCAL-SYNC" || !bytes.Equal(cfg.LocalSyncthingIdentity, []byte("local-identity")) {
		t.Fatalf("config after retry = %#v", cfg)
	}
	if _, err := f.secrets.Get(f.deviceID, sshtransport.SSHPrivateKeyCredential); !errors.Is(err, credentials.ErrNotFound) {
		t.Fatalf("credential after retry error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "ssh_config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ssh_config after retry error = %v", err)
	}
	knownHosts, err := os.ReadFile(filepath.Join(f.root, "known_hosts"))
	if err != nil || string(knownHosts) != "unrelated ssh-ed25519 unrelated-key\n" {
		t.Fatalf("known_hosts after retry = %q, error = %v", knownHosts, err)
	}
}

type failingDeleteCredentialStore struct {
	credentials.Store
	deleteFailures int
}

func (s *failingDeleteCredentialStore) Delete(deviceID, name string) error {
	if s.deleteFailures > 0 {
		s.deleteFailures--
		return errors.New("injected keyring deletion failure")
	}
	return s.Store.Delete(deviceID, name)
}

func testManagedSSHRoot(t *testing.T, path string) sshtransport.ManagedRoot {
	t.Helper()
	root, err := sshtransport.NewManagedRoot(path)
	if err != nil {
		t.Fatalf("NewManagedRoot() error = %v", err)
	}
	return root
}

func TestMacPairingCandidatesKeepUnavailableTrustedDevice(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		ActiveDevice:  "trusted-windows",
		Devices: map[string]config.Device{
			"trusted-windows": {Name: "Saved Windows", Address: "192.168.1.20", SSHPort: 49222},
		},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Transport: &runtimePairingTransport{targets: []pairingTarget{}},
	})
	candidates, err := coordinator.Candidates(context.Background())
	if err != nil {
		t.Fatalf("Candidates() error = %v", err)
	}
	want := []localapi.PairingCandidate{{
		ID: "trusted-windows", Name: "Saved Windows", Trusted: true, Available: false,
	}}
	if !reflect.DeepEqual(candidates.Candidates, want) {
		t.Fatalf("candidates = %#v, want %#v", candidates.Candidates, want)
	}
}

func TestMacPairingCandidatesMergeStableTrustedAdvertisement(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		ActiveDevice:  "trusted-windows",
		Devices: map[string]config.Device{
			"trusted-windows": {Name: "Saved Windows", Address: "192.168.1.20", SSHPort: 49222},
		},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store,
		Transport: &runtimePairingTransport{targets: []pairingTarget{{
			InstanceID: "trusted-windows", Name: "Untrusted mDNS name", Address: "10.0.0.20",
			TrustedAdvertisement: true,
		}}},
	})
	candidates, err := coordinator.Candidates(context.Background())
	if err != nil {
		t.Fatalf("Candidates() error = %v", err)
	}
	want := []localapi.PairingCandidate{{
		ID: "trusted-windows", Name: "Saved Windows", Trusted: true, Available: true,
	}}
	if !reflect.DeepEqual(candidates.Candidates, want) {
		t.Fatalf("candidates = %#v, want %#v", candidates.Candidates, want)
	}
}

func TestMacPairingCoordinatorCancelsWithoutManualCodeAndClearsPendingSecret(t *testing.T) {
	root := t.TempDir()
	transport := &runtimePairingTransport{hostKey: testAuthorizedKey(t), status: pairing.SessionPending}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: config.Store{Path: filepath.Join(root, "config.json")}, Secrets: credentials.NewMemoryStore(),
		Transport: transport, Docker: &runtimeDockerExecutor{}, DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:  filepath.Join(root, "ssh_config"), KnownHostsPath: filepath.Join(root, "known_hosts"),
		AgentSocketPath: filepath.Join(root, "ssh-agent.sock"), ControlDir: filepath.Join(root, "control"),
	})
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := coordinator.Cancel(context.Background(), started.SessionID); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	if transport.cancelled != started.SessionID {
		t.Fatalf("cancelled session = %q, want %q", transport.cancelled, started.SessionID)
	}
	coordinator.mu.Lock()
	pending := coordinator.pending
	coordinator.mu.Unlock()
	if pending != nil {
		t.Fatalf("pending pairing survived cancellation: %#v", pending)
	}
}

func TestMacPairingCancelCurrentBeforeRemoteConfirmAbandonsExactGeneration(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	transport := &runtimePairingTransport{hostKey: testAuthorizedKey(t), status: pairing.SessionPending}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: &runtimeDockerExecutor{},
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:  filepath.Join(root, "ssh_config"), KnownHostsPath: filepath.Join(root, "known_hosts"),
	})
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	before, err := store.Load()
	if err != nil || len(before.PendingRevocations) != 1 {
		t.Fatalf("initial rollback journal = %#v error=%v", before.PendingRevocations, err)
	}
	var proofOwner string
	for _, pending := range before.PendingRevocations {
		proofOwner = pending.Device.RevocationCredentialOwner
	}

	result, err := coordinator.CancelCurrent(context.Background(), started.SessionID)
	if err != nil || result.Status != string(pairing.SessionCancelled) {
		t.Fatalf("CancelCurrent() = %#v error=%v", result, err)
	}
	if transport.cancelCalls != 1 || transport.revokeCalls != 0 {
		t.Fatalf("remote cancel calls=%d revoke calls=%d", transport.cancelCalls, transport.revokeCalls)
	}
	after, err := store.Load()
	if err != nil || after.ActiveDevice != "" || len(after.Devices) != 0 || len(after.PendingRevocations) != 0 {
		t.Fatalf("state after pre-confirm cancel = %#v error=%v", after, err)
	}
	if _, err := secrets.Get(proofOwner, revocationProofCredential); !errors.Is(err, credentials.ErrNotFound) {
		t.Fatalf("rollback proof survived abandoned generation: %v", err)
	}
}

func TestMacPairingCancelCurrentWinsAgainstInflightRemoteConfirm(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	transport := &cancelWinsConfirmPairingTransport{
		runtimePairingTransport: &runtimePairingTransport{hostKey: testAuthorizedKey(t)},
		confirmStarted:          make(chan struct{}), releaseConfirm: make(chan struct{}),
	}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: &runtimeDockerExecutor{},
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
	})
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	completeDone := make(chan error, 1)
	go func() {
		_, completeErr := coordinator.Status(context.Background(), started.SessionID)
		completeDone <- completeErr
	}()
	waitForTestSignal(t, transport.confirmStarted, "remote confirm")
	result, err := coordinator.CancelCurrent(context.Background(), started.SessionID)
	if err != nil || result.Status != string(pairing.SessionCancelled) {
		t.Fatalf("CancelCurrent() = %#v error=%v", result, err)
	}
	close(transport.releaseConfirm)
	if err := waitForTestError(t, completeDone, "cancelled remote confirm"); err == nil {
		t.Fatal("in-flight completion error = nil after cancellation won")
	}
	cfg, err := store.Load()
	if err != nil || cfg.ActiveDevice != "" || len(cfg.Devices) != 0 || len(cfg.PendingRevocations) != 0 {
		t.Fatalf("state after cancellation won = %#v error=%v", cfg, err)
	}
}

func TestMacPairingCancelRetainsRevokeAuthorityUntilDelayedConfirmQuiesces(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	transport := &delayedConfirmAfterNoopRevokeTransport{
		runtimePairingTransport: &runtimePairingTransport{hostKey: testAuthorizedKey(t)},
		confirmStarted:          make(chan struct{}), releaseConfirm: make(chan struct{}), confirmDone: make(chan struct{}),
	}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: &runtimeDockerExecutor{},
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID:     func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		CleanupTaskTimeout: 30 * time.Millisecond, RemoteCleanupTimeout: 10 * time.Millisecond,
	})
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	before, err := store.Load()
	if err != nil || len(before.PendingRevocations) != 1 {
		t.Fatalf("initial rollback journal = %#v error=%v", before.PendingRevocations, err)
	}
	proofOwner := ""
	generation := ""
	for id, pending := range before.PendingRevocations {
		generation = id
		proofOwner = pending.Device.RevocationCredentialOwner
	}

	completeDone := make(chan error, 1)
	go func() {
		_, completeErr := coordinator.Status(context.Background(), started.SessionID)
		completeDone <- completeErr
	}()
	waitForTestSignal(t, transport.confirmStarted, "delayed Windows confirm")
	cancelCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	begin := time.Now()
	result, cancelErr := coordinator.CancelCurrent(cancelCtx, started.SessionID)
	elapsed := time.Since(begin)
	cancel()
	if cancelErr != nil || result.Status != string(pairing.SessionCancelled) {
		t.Fatalf("CancelCurrent() = %#v error=%v", result, cancelErr)
	}
	afterCancel, loadErr := store.Load()
	journalRetained := loadErr == nil && len(afterCancel.PendingRevocations) == 1
	_, proofErr := secrets.Get(proofOwner, revocationProofCredential)

	close(transport.releaseConfirm)
	_ = waitForTestError(t, completeDone, "delayed Windows confirm")
	if err := coordinator.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("ReconcilePendingRevocations() error = %v", err)
	}
	afterRetry, retryErr := store.Load()
	if elapsed > 500*time.Millisecond {
		t.Fatalf("local cancellation took %s", elapsed)
	}
	if !journalRetained || proofErr != nil {
		t.Fatalf("cancel lost revoke authority before Confirm quiesced: journal=%#v proof=%v load=%v", afterCancel.PendingRevocations, proofErr, loadErr)
	}
	if transport.Trusted() || transport.RevokeCalls() < 2 || transport.LastRevokeGeneration() != generation {
		t.Fatalf("late Windows trust survived exact-generation retry: trusted=%t revokes=%d generation=%q want=%q",
			transport.Trusted(), transport.RevokeCalls(), transport.LastRevokeGeneration(), generation)
	}
	if retryErr != nil || len(afterRetry.PendingRevocations) != 0 {
		t.Fatalf("retry cleanup journal = %#v error=%v", afterRetry.PendingRevocations, retryErr)
	}
}

func TestMacPairingCancelCurrentPreservesFullyCommittedTrust(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	docker := &runtimeDockerExecutor{}
	transport := &runtimePairingTransport{hostKey: testAuthorizedKey(t)}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: docker,
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:  filepath.Join(root, "ssh_config"), ManagedSSHRoot: testManagedSSHRoot(t, root),
		KnownHostsPath: filepath.Join(root, "known_hosts"), AgentSocketPath: filepath.Join(root, "ssh-agent.sock"),
		ControlDir: filepath.Join(root, "control"),
	})
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	completed, err := coordinator.Status(context.Background(), started.SessionID)
	if err != nil || completed.Device == nil {
		t.Fatalf("Status() = %#v error=%v", completed, err)
	}
	callsBeforeCancel := len(docker.calls)

	result, err := coordinator.CancelCurrent(context.Background(), started.SessionID)
	if err != nil || result.Status != string(pairing.SessionCompleted) || result.Device == nil || result.Device.ID != completed.Device.ID {
		t.Fatalf("CancelCurrent() = %#v error=%v", result, err)
	}
	cfg, err := store.Load()
	device, exists := cfg.Devices[completed.Device.ID]
	if err != nil || !exists || cfg.ActiveDevice != completed.Device.ID || len(cfg.PendingRevocations) != 0 {
		t.Fatalf("committed trust after cancel = %#v error=%v", cfg, err)
	}
	if transport.cancelCalls != 0 || transport.revokeCalls != 0 || len(docker.calls) != callsBeforeCancel {
		t.Fatalf("committed artifacts were touched: cancel=%d revoke=%d docker=%#v", transport.cancelCalls, transport.revokeCalls, docker.calls)
	}
	for _, credential := range []string{sshtransport.SSHPrivateKeyCredential, tunnel.IdentityCredential} {
		if _, err := secrets.Get(completed.Device.ID, credential); err != nil {
			t.Fatalf("committed credential %q was removed: %v", credential, err)
		}
	}
	if device.PairingGeneration == "" {
		t.Fatal("committed device lost its pairing generation")
	}
}

func TestMacPairingDurableCommitWinsAgainstConcurrentCancelCurrent(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	transport := &runtimePairingTransport{hostKey: testAuthorizedKey(t)}
	commitStarted := make(chan struct{})
	releaseCommit := make(chan struct{})
	var commitOnce sync.Once
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: &runtimeDockerExecutor{},
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:  filepath.Join(root, "ssh_config"), ManagedSSHRoot: testManagedSSHRoot(t, root),
		KnownHostsPath: filepath.Join(root, "known_hosts"), AgentSocketPath: filepath.Join(root, "ssh-agent.sock"),
		ControlDir: filepath.Join(root, "control"),
		SaveConfig: func(cfg config.Config) error {
			if cfg.ActiveDevice != "" {
				commitOnce.Do(func() { close(commitStarted) })
				<-releaseCommit
			}
			return store.Save(cfg)
		},
	})
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	completeDone := make(chan localapi.PairingStatusResult, 1)
	completeErr := make(chan error, 1)
	go func() {
		result, statusErr := coordinator.Status(context.Background(), started.SessionID)
		completeDone <- result
		completeErr <- statusErr
	}()
	waitForTestSignal(t, commitStarted, "durable pairing commit")
	cancelDone := make(chan localapi.PairingStatusResult, 1)
	cancelErr := make(chan error, 1)
	go func() {
		result, currentErr := coordinator.CancelCurrent(context.Background(), started.SessionID)
		cancelDone <- result
		cancelErr <- currentErr
	}()
	select {
	case result := <-cancelDone:
		t.Fatalf("CancelCurrent() returned before durable commit resolved: %#v", result)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseCommit)
	completed := <-completeDone
	if err := <-completeErr; err != nil || completed.Device == nil {
		t.Fatalf("Status() = %#v error=%v", completed, err)
	}
	cancelled := <-cancelDone
	if err := <-cancelErr; err != nil || cancelled.Status != string(pairing.SessionCompleted) ||
		cancelled.Device == nil || cancelled.Device.ID != completed.Device.ID {
		t.Fatalf("CancelCurrent() after commit = %#v error=%v", cancelled, err)
	}
	cfg, err := store.Load()
	if err != nil || cfg.ActiveDevice != completed.Device.ID || len(cfg.PendingRevocations) != 0 || transport.revokeCalls != 0 {
		t.Fatalf("committed state after concurrent cancel = %#v revoke=%d error=%v", cfg, transport.revokeCalls, err)
	}
}

func TestMacPairingCancelCurrentRetainsConfirmedGenerationAfterLocalCommitFailure(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	transport := &runtimePairingTransport{
		hostKey: testAuthorizedKey(t), revokeErr: errors.New("windows offline"),
	}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: &runtimeDockerExecutor{},
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:  filepath.Join(root, "ssh_config"), ManagedSSHRoot: testManagedSSHRoot(t, root),
		KnownHostsPath: filepath.Join(root, "known_hosts"), AgentSocketPath: filepath.Join(root, "ssh-agent.sock"),
		ControlDir: filepath.Join(root, "control"),
		SaveConfig: func(cfg config.Config) error {
			if cfg.ActiveDevice != "" {
				return errors.New("injected local durable commit failure")
			}
			return store.Save(cfg)
		},
	})
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := coordinator.Status(context.Background(), started.SessionID); err == nil {
		t.Fatal("Status() error = nil, want local durable commit failure")
	}
	before, err := store.Load()
	if err != nil || len(before.PendingRevocations) != 1 {
		t.Fatalf("half-committed journal = %#v error=%v", before.PendingRevocations, err)
	}
	var generation string
	for id, pending := range before.PendingRevocations {
		generation = id
		if pending.Generation != id || pending.SessionID != started.SessionID || pending.RemoteRevoked || !pending.CleanupRequested {
			t.Fatalf("half-committed journal = %#v", pending)
		}
	}

	result, err := coordinator.CancelCurrent(context.Background(), started.SessionID)
	if err != nil || result.Status != string(pairing.SessionCancelled) {
		t.Fatalf("CancelCurrent() = %#v error=%v", result, err)
	}
	after, err := store.Load()
	pending, exists := after.PendingRevocations[generation]
	if err != nil || !exists || pending.Generation != generation || pending.RemoteRevoked || !pending.CleanupRequested {
		t.Fatalf("retryable exact-generation cleanup = %#v exists=%t error=%v", pending, exists, err)
	}
	if transport.revokeCalls < 1 {
		t.Fatal("confirmed remote generation was not scheduled for proof-protected revoke")
	}
}

func TestMacPairingCoordinatorPersistsCancelRetryAfterRemoteFailure(t *testing.T) {
	root := t.TempDir()
	transport := &runtimePairingTransport{
		hostKey: testAuthorizedKey(t), status: pairing.SessionPending,
		cancelErr: errors.New("remote unavailable"), revokeErr: errors.New("remote unavailable"),
	}
	store := config.Store{Path: filepath.Join(root, "config.json")}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: credentials.NewMemoryStore(),
		Transport: transport, Docker: &runtimeDockerExecutor{}, DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
	})
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := coordinator.Cancel(context.Background(), started.SessionID); err != nil {
		t.Fatalf("Cancel() local completion error = %v", err)
	}
	coordinator.mu.Lock()
	pending := coordinator.pending
	starting := coordinator.starting
	coordinator.mu.Unlock()
	if pending != nil || starting {
		t.Fatalf("volatile pairing after failed remote cancel: pending=%#v starting=%t", pending, starting)
	}
	cfg, err := store.Load()
	if err != nil || len(cfg.PendingRevocations) != 1 {
		t.Fatalf("durable retry journal = %#v error=%v", cfg.PendingRevocations, err)
	}
	transport.revokeErr = nil
	transport.status = pairing.SessionCancelled
	if err := coordinator.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("durable cancel retry error = %v", err)
	}
	cfg, err = store.Load()
	if err != nil || len(cfg.PendingRevocations) != 0 || transport.cancelCalls != 1 || transport.revokeCalls != 3 {
		t.Fatalf("retry result journals=%#v cancel=%d revoke=%d error=%v", cfg.PendingRevocations, transport.cancelCalls, transport.revokeCalls, err)
	}
}

func TestMacPairingCancelOfflinePeerTerminatesLocallyWithDurableRetry(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	transport := &offlineCancellationPairingTransport{
		runtimePairingTransport: &runtimePairingTransport{hostKey: testAuthorizedKey(t), status: pairing.SessionPending},
	}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: &runtimeDockerExecutor{},
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID:     func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		CleanupTaskTimeout: 100 * time.Millisecond, RemoteCleanupTimeout: 10 * time.Millisecond,
	})
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	cancelCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	begin := time.Now()
	result, err := coordinator.Cancel(cancelCtx, started.SessionID)
	if err != nil || result.Status != string(pairing.SessionCancelled) {
		t.Fatalf("Cancel() = %#v error=%v", result, err)
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("offline cancellation took %s", elapsed)
	}
	coordinator.mu.Lock()
	pending := coordinator.pending
	coordinator.mu.Unlock()
	if pending != nil {
		t.Fatalf("offline cancellation retained volatile pairing = %#v", pending)
	}
	cfg, err := store.Load()
	if err != nil || len(cfg.PendingRevocations) != 1 {
		t.Fatalf("durable retry journal = %#v error=%v", cfg.PendingRevocations, err)
	}
	for _, journal := range cfg.PendingRevocations {
		if !journal.CleanupRequested || journal.RemoteRevoked || journal.CompletionLeaseToken != "" || journal.CleanupLeaseToken != "" {
			t.Fatalf("offline retry journal = %#v", journal)
		}
	}
}

func TestMacPairingInvalidBootstrapDescriptorUsesDetachedCancellation(t *testing.T) {
	root := t.TempDir()
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	transport := &runtimePairingTransport{invalidDescriptor: true, afterBootstrap: cancelRequest}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: config.Store{Path: filepath.Join(root, "config.json")}, Secrets: credentials.NewMemoryStore(),
		Transport: transport, Docker: &runtimeDockerExecutor{}, DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
	})

	if _, err := coordinator.Start(requestCtx, "windows-peer"); err == nil {
		t.Fatal("Start() invalid descriptor error = nil")
	}
	if transport.cancelCalls != 1 || transport.cancelCtxErr != nil {
		t.Fatalf("invalid descriptor cancellation calls=%d context error=%v", transport.cancelCalls, transport.cancelCtxErr)
	}
	coordinator.mu.Lock()
	pending := coordinator.pending
	cancellation := coordinator.cancellation
	coordinator.mu.Unlock()
	if pending != nil || cancellation != nil {
		t.Fatalf("successful invalid-descriptor cancellation retained local state: pending=%#v cancellation=%#v", pending, cancellation)
	}
}

func TestMacPairingInvalidDescriptorCancellationBlocksBootstrapUntilRetried(t *testing.T) {
	root := t.TempDir()
	transport := &runtimePairingTransport{
		invalidDescriptor: true,
		cancelErrors:      []error{errors.New("cancel unavailable"), errors.New("still unavailable"), nil},
	}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: config.Store{Path: filepath.Join(root, "config.json")}, Secrets: credentials.NewMemoryStore(),
		Transport: transport, Docker: &runtimeDockerExecutor{}, DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
	})

	if _, err := coordinator.Start(context.Background(), "windows-peer"); err == nil {
		t.Fatal("first Start() invalid descriptor error = nil")
	}
	transport.invalidDescriptor = false
	if _, err := coordinator.Start(context.Background(), "windows-peer"); err == nil {
		t.Fatal("second Start() error = nil while cancellation remains unconfirmed")
	}
	if transport.bootstrapCalls != 1 {
		t.Fatalf("second Bootstrap ran before cancellation acknowledgement: calls=%d", transport.bootstrapCalls)
	}
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("third Start() after cancellation retry error = %v", err)
	}
	if started.SessionID == "" || transport.bootstrapCalls != 2 || transport.cancelCalls != 3 {
		t.Fatalf("retry result=%#v bootstrap=%d cancel=%d", started, transport.bootstrapCalls, transport.cancelCalls)
	}
	wantEvents := []string{"bootstrap", "cancel", "cancel", "cancel", "bootstrap"}
	if !reflect.DeepEqual(transport.events, wantEvents) {
		t.Fatalf("cancellation retry order = %v, want %v", transport.events, wantEvents)
	}
}

func TestMacPairingCoordinatorReservesStartBeforeRemoteBootstrap(t *testing.T) {
	root := t.TempDir()
	transport := &blockingBootstrapPairingTransport{
		runtimePairingTransport: &runtimePairingTransport{hostKey: testAuthorizedKey(t)},
		started:                 make(chan struct{}),
		release:                 make(chan struct{}),
	}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: config.Store{Path: filepath.Join(root, "config.json")}, Secrets: credentials.NewMemoryStore(),
		Transport: transport, Docker: &runtimeDockerExecutor{}, DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
	})

	firstDone := make(chan error, 1)
	go func() {
		_, err := coordinator.Start(context.Background(), "windows-peer")
		firstDone <- err
	}()
	waitForTestSignal(t, transport.started, "remote bootstrap")
	secondDone := make(chan error, 1)
	go func() {
		_, err := coordinator.Start(context.Background(), "windows-peer")
		secondDone <- err
	}()
	if err := waitForTestError(t, secondDone, "reserved concurrent start"); err == nil {
		t.Fatal("second Start() error = nil")
	}
	if transport.BootstrapCalls() != 1 {
		t.Fatalf("remote bootstrap calls = %d, want one", transport.BootstrapCalls())
	}
	close(transport.release)
	if err := waitForTestError(t, firstDone, "first start"); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
}

func TestMacPairingRefusesLegacyManagedDockerContext(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	transport := &runtimePairingTransport{hostKey: testAuthorizedKey(t)}
	docker := &managedContextExecutor{
		host: "ssh://remote-docker-device-old-device", description: "Managed by Remote Docker",
	}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: docker,
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID:  func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:   filepath.Join(root, "ssh_config"),
		ManagedSSHRoot:  testManagedSSHRoot(t, root),
		KnownHostsPath:  filepath.Join(root, "known_hosts"),
		AgentSocketPath: filepath.Join(root, "ssh-agent.sock"),
		ControlDir:      filepath.Join(root, "control"),
	})

	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("start pairing: %v", err)
	}
	if _, err := coordinator.Status(context.Background(), started.SessionID); err == nil {
		t.Fatal("Status() error = nil, want legacy Docker context conflict")
	}
	if got := docker.Commands(); !reflect.DeepEqual(got, [][]string{{"context", "inspect", "remote-docker"}}) {
		t.Fatalf("legacy Docker context was mutated: %#v", got)
	}
}

func TestMacPairingReplacementNeverChangesForeignDockerContext(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	transport := &runtimePairingTransport{hostKey: testAuthorizedKey(t)}
	docker := &managedContextExecutor{host: "ssh://user-owned-host", description: "Created by user"}
	secrets := credentials.NewMemoryStore()
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: secrets, Transport: transport, Docker: docker,
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID:  func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:   filepath.Join(root, "ssh_config"),
		ManagedSSHRoot:  testManagedSSHRoot(t, root),
		KnownHostsPath:  filepath.Join(root, "known_hosts"),
		AgentSocketPath: filepath.Join(root, "ssh-agent.sock"),
		ControlDir:      filepath.Join(root, "control"),
	})

	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := coordinator.Status(context.Background(), started.SessionID); err == nil {
		t.Fatal("Status() error = nil, want foreign context collision")
	}
	if got := docker.Commands(); !reflect.DeepEqual(got, [][]string{{"context", "inspect", "remote-docker"}}) {
		t.Fatalf("foreign Docker context was mutated: %#v", got)
	}
	cfg, err := loadAgentConfig(store)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.ActiveDevice != "" || len(cfg.Devices) != 0 {
		t.Fatalf("trust committed after context collision: %#v", cfg)
	}
}

func TestMacPairingDoesNotDeleteContextCreatedAfterDockerPlan(t *testing.T) {
	root := t.TempDir()
	docker := &contextAppearsAfterPlanExecutor{foreignHost: "ssh://remote-docker-device-other"}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: config.Store{Path: filepath.Join(root, "config.json")}, Secrets: credentials.NewMemoryStore(),
		Transport: &runtimePairingTransport{hostKey: testAuthorizedKey(t)}, Docker: docker,
		DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID:  func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:   filepath.Join(root, "ssh_config"),
		ManagedSSHRoot:  testManagedSSHRoot(t, root),
		KnownHostsPath:  filepath.Join(root, "known_hosts"),
		AgentSocketPath: filepath.Join(root, "ssh-agent.sock"),
		ControlDir:      filepath.Join(root, "control"),
	})
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := coordinator.Status(context.Background(), started.SessionID); err == nil {
		t.Fatal("Status() error = nil, want changed Docker precondition")
	}
	if docker.dangerousMutation || docker.host != docker.foreignHost || docker.createCalls != 1 {
		t.Fatalf("foreign context changed: host=%q dangerous=%t creates=%d commands=%#v", docker.host, docker.dangerousMutation, docker.createCalls, docker.commands)
	}
}

func TestMacPairingRemovesExactCreatedContextWhenLocalCommitFails(t *testing.T) {
	root := t.TempDir()
	transport := &runtimePairingTransport{hostKey: testAuthorizedKey(t)}
	docker := &managedContextExecutor{}
	secrets := credentials.NewMemoryStore()
	saves := 0
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: config.Store{Path: filepath.Join(root, "config.json")}, Secrets: secrets,
		Transport: transport, Docker: docker, DockerCLI: "docker-real", DockerContext: "remote-docker",
		ClientDeviceID:  func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:   filepath.Join(root, "ssh_config"),
		ManagedSSHRoot:  testManagedSSHRoot(t, root),
		KnownHostsPath:  filepath.Join(root, "known_hosts"),
		AgentSocketPath: filepath.Join(root, "ssh-agent.sock"),
		ControlDir:      filepath.Join(root, "control"),
		SaveConfig: func(cfg config.Config) error {
			saves++
			if saves == 5 {
				return errors.New("injected local commit failure")
			}
			return (config.Store{Path: filepath.Join(root, "config.json")}).Save(cfg)
		},
	})
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := coordinator.Status(context.Background(), started.SessionID); err == nil {
		t.Fatal("Status() error = nil, want local commit failure")
	}
	commands := docker.Commands()
	if len(commands) != 7 ||
		!reflect.DeepEqual(commands[0], []string{"context", "inspect", "remote-docker"}) ||
		!reflect.DeepEqual(commands[1], []string{"context", "inspect", "remote-docker"}) ||
		commands[2][1] != "create" ||
		!reflect.DeepEqual(commands[3], []string{"context", "inspect", "remote-docker"}) ||
		!reflect.DeepEqual(commands[4], []string{"context", "inspect", "remote-docker"}) ||
		!reflect.DeepEqual(commands[5], []string{"context", "rm", "--force", "remote-docker"}) ||
		!reflect.DeepEqual(commands[6], []string{"context", "inspect", "remote-docker"}) {
		t.Fatalf("Docker context rollback commands = %#v", commands)
	}
}

func TestMacPairingPersistsDockerRollbackIntentBeforeContextMutation(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	locker := &trackingStateLocker{}
	docker := &dockerIntentCheckingExecutor{beforeMutation: func() error {
		if !locker.held {
			return errors.New("Docker context process lock is not held")
		}
		cfg, err := store.Load()
		if err != nil {
			return err
		}
		for _, pending := range cfg.PendingRevocations {
			if pending.DockerContext.Name == "remote-docker" &&
				strings.HasPrefix(pending.DockerContext.CurrentHost, "ssh://remote-docker-device-") {
				return nil
			}
		}
		return errors.New("Docker rollback intent is not durable")
	}}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: credentials.NewMemoryStore(),
		Transport: &runtimePairingTransport{hostKey: testAuthorizedKey(t)}, Docker: docker,
		ConfigTransactions: &configTransactions{locker: locker},
		DockerCLI:          "docker-real", DockerContext: "remote-docker",
		ClientDeviceID: func(context.Context) (string, error) { return "LOCAL-SYNC", nil },
		SSHConfigPath:  filepath.Join(root, "ssh_config"), ManagedSSHRoot: testManagedSSHRoot(t, root),
		KnownHostsPath: filepath.Join(root, "known_hosts"), AgentSocketPath: filepath.Join(root, "ssh-agent.sock"),
		ControlDir: filepath.Join(root, "control"),
	})
	started, err := coordinator.Start(context.Background(), "windows-peer")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	status, err := coordinator.Status(context.Background(), started.SessionID)
	if err != nil || status.Device == nil {
		t.Fatalf("Status() = %#v error=%v", status, err)
	}
	if !docker.mutated {
		t.Fatal("Docker context was not mutated")
	}
	if locker.entries == 0 || locker.held {
		t.Fatalf("Docker context lock entries=%d held=%t", locker.entries, locker.held)
	}
}

func TestDockerContextChangeConfigRoundTripPreservesOwnership(t *testing.T) {
	want := dockercli.ContextChange{
		Name: "remote-docker", PreviousHost: "ssh://remote-docker-device-old",
		PreviousDescription: "Managed by Remote Docker; owner=owner-old",
		CurrentHost:         "ssh://remote-docker-device-new", OwnerToken: "owner-new",
	}
	if got := contextChangeFromConfig(contextChangeToConfig(want)); !reflect.DeepEqual(got, want) {
		t.Fatalf("context ownership round trip = %#v, want %#v", got, want)
	}
}

func TestRestartedCleanupTreatsDockerOwnershipLossAsTerminal(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	host := "ssh://remote-docker-device-peer"
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		PendingRevocations: map[string]config.PendingRevocation{"cleanup-a": {
			Generation: "cleanup-a", CleanupRequested: true, RemoteRevoked: true, LocalCleaned: true,
			DockerContext: config.DockerContextChange{
				Name: "remote-docker", CurrentHost: host, OwnerToken: "cleanup-a", Created: true,
			},
		}},
	}); err != nil {
		t.Fatalf("seed pending cleanup: %v", err)
	}
	locker := &trackingStateLocker{}
	docker := &managedContextExecutor{host: host, description: "Managed by Remote Docker; owner=cleanup-b"}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: credentials.NewMemoryStore(), Docker: docker, DockerCLI: "docker-real",
		ConfigTransactions: &configTransactions{locker: locker},
	})

	if err := coordinator.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("ReconcilePendingRevocations() error = %v", err)
	}
	cfg, err := store.Load()
	if err != nil || len(cfg.PendingRevocations) != 0 {
		t.Fatalf("terminal ownership-loss cleanup = %#v error=%v", cfg.PendingRevocations, err)
	}
	for _, command := range docker.Commands() {
		if len(command) >= 2 && (command[1] == "rm" || command[1] == "update") {
			t.Fatalf("foreign Docker context was mutated: %#v", docker.Commands())
		}
	}
	if locker.entries == 0 || locker.held {
		t.Fatalf("recovery lock entries=%d held=%t", locker.entries, locker.held)
	}
}

func TestRestartAfterDockerPreconditionFailureDoesNotDeleteForeignContext(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	cleanupID := "cleanup-a"
	completionToken := "completion-a"
	host := "ssh://remote-docker-device-peer"
	firstDocker := &contextCreatedBeforeApplyExecutor{host: host, description: "Managed by Remote Docker; owner=cleanup-b"}
	first := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: credentials.NewMemoryStore(), Docker: firstDocker, DockerCLI: "docker-real", DockerContext: "remote-docker",
		KnownHostsPath: filepath.Join(root, "known_hosts"), SSHConfigPath: filepath.Join(root, "ssh_config"),
		ManagedSSHRoot: testManagedSSHRoot(t, root), AgentSocketPath: filepath.Join(root, "agent.sock"), ControlDir: filepath.Join(root, "control"),
	})
	if err := first.persistPendingRevocation(context.Background(), cleanupID, config.PendingRevocation{
		Generation: cleanupID, RemoteRevoked: true, LocalCleaned: true,
		CompletionLeaseToken: completionToken, CompletionLeaseExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("persist initial cleanup journal: %v", err)
	}
	pending := pendingPairing{cleanupID: cleanupID, completionLeaseToken: completionToken}
	_, change, err := first.commitPairingArtifacts(context.Background(), pending, "peer", pairing.DeviceRecord{
		SSHHostPublicKey: testAuthorizedKey(t),
	}, config.Device{})
	if err == nil || !change.Created || change.OwnerToken != cleanupID {
		t.Fatalf("commitPairingArtifacts() change=%#v error=%v, want durable failed create plan", change, err)
	}
	if err := first.requestPendingCleanup(cleanupID); err != nil {
		t.Fatalf("activate cleanup after failed commit: %v", err)
	}
	cfg, loadErr := store.Load()
	if loadErr != nil || cfg.PendingRevocations[cleanupID].DockerContext.OwnerToken != cleanupID {
		t.Fatalf("rollback journal after precondition failure = %#v error=%v", cfg.PendingRevocations[cleanupID], loadErr)
	}

	restartedDocker := &managedContextExecutor{host: host, description: "Managed by Remote Docker; owner=cleanup-b"}
	restarted := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: credentials.NewMemoryStore(), Docker: restartedDocker, DockerCLI: "docker-real",
	})
	if err := restarted.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("restarted ReconcilePendingRevocations() error = %v", err)
	}
	for _, command := range restartedDocker.Commands() {
		if len(command) >= 2 && (command[1] == "rm" || command[1] == "update") {
			t.Fatalf("foreign context was mutated after restart: %#v", restartedDocker.Commands())
		}
	}
}

func TestRestartAfterUnknownDockerCreateDoesNotDeleteForeignContext(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	cleanupID := "cleanup-a"
	completionToken := "completion-a"
	host := "ssh://remote-docker-device-peer"
	firstDocker := &unknownCreateResultExecutor{}
	first := newMacPairingCoordinator(macPairingOptions{
		Store: store, ConfigTransactions: newConfigTransactions(store.Path), Secrets: credentials.NewMemoryStore(),
		Docker: firstDocker, DockerCLI: "docker-real", DockerContext: "remote-docker",
		KnownHostsPath: filepath.Join(root, "known_hosts"), SSHConfigPath: filepath.Join(root, "ssh_config"),
		ManagedSSHRoot: testManagedSSHRoot(t, root), AgentSocketPath: filepath.Join(root, "agent.sock"), ControlDir: filepath.Join(root, "control"),
	})
	if err := first.persistPendingRevocation(context.Background(), cleanupID, config.PendingRevocation{
		Generation: cleanupID, RemoteRevoked: true, LocalCleaned: true,
		CompletionLeaseToken: completionToken, CompletionLeaseExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("persist initial cleanup journal: %v", err)
	}
	_, change, err := first.commitPairingArtifacts(context.Background(), pendingPairing{
		cleanupID: cleanupID, completionLeaseToken: completionToken,
	}, "peer", pairing.DeviceRecord{
		SSHHostPublicKey: testAuthorizedKey(t),
	}, config.Device{})
	if err == nil || !errors.Is(firstDocker.createErr, context.Canceled) || !change.Created || change.OwnerToken != cleanupID {
		t.Fatalf("unknown Docker create change=%#v createErr=%v error=%v", change, firstDocker.createErr, err)
	}
	if err := first.requestPendingCleanup(cleanupID); err != nil {
		t.Fatalf("activate cleanup after unknown create: %v", err)
	}
	cfg, loadErr := store.Load()
	pending, exists := cfg.PendingRevocations[cleanupID]
	if loadErr != nil || !exists || pending.DockerContext.OwnerToken != cleanupID || pending.DockerRestored {
		t.Fatalf("durable unknown-result journal = %#v exists=%t error=%v", pending, exists, loadErr)
	}

	restartedDocker := &managedContextExecutor{host: host, description: "Managed by Remote Docker; owner=cleanup-b"}
	restarted := newMacPairingCoordinator(macPairingOptions{
		Store: store, ConfigTransactions: newConfigTransactions(store.Path), Secrets: credentials.NewMemoryStore(),
		Docker: restartedDocker, DockerCLI: "docker-real",
	})
	if err := restarted.ReconcilePendingRevocations(context.Background()); err != nil {
		t.Fatalf("restart recovery: %v", err)
	}
	for _, command := range restartedDocker.Commands() {
		if len(command) >= 2 && (command[1] == "rm" || command[1] == "update") {
			t.Fatalf("foreign context was mutated after unknown create: %#v", restartedDocker.Commands())
		}
	}
	cfg, loadErr = store.Load()
	if loadErr != nil || len(cfg.PendingRevocations) != 0 {
		t.Fatalf("proven ownership mismatch was not terminal: %#v error=%v", cfg.PendingRevocations, loadErr)
	}
}

func TestRestartedCleanupKeepsJournalWhenDockerInspectIsAmbiguous(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	host := "ssh://remote-docker-device-peer"
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		PendingRevocations: map[string]config.PendingRevocation{"cleanup-a": {
			Generation: "cleanup-a", CleanupRequested: true, RemoteRevoked: true, LocalCleaned: true,
			DockerContext: config.DockerContextChange{
				Name: "remote-docker", CurrentHost: host, OwnerToken: "cleanup-a", Created: true,
			},
		}},
	}); err != nil {
		t.Fatalf("seed pending cleanup: %v", err)
	}
	coordinator := newMacPairingCoordinator(macPairingOptions{
		Store: store, Secrets: credentials.NewMemoryStore(), Docker: inspectFailureDockerExecutor{}, DockerCLI: "docker-real",
	})

	if err := coordinator.ReconcilePendingRevocations(context.Background()); err == nil {
		t.Fatal("ReconcilePendingRevocations() error = nil, want ambiguous inspect failure")
	}
	cfg, err := store.Load()
	pending, exists := cfg.PendingRevocations["cleanup-a"]
	if err != nil || !exists || pending.DockerRestored || pending.DockerContext.OwnerToken != "cleanup-a" {
		t.Fatalf("ambiguous inspect discarded rollback journal: pending=%#v exists=%t error=%v", pending, exists, err)
	}
}

type trackingStateLocker struct {
	held    bool
	entries int
}

func seedPendingRemoteCleanup(t *testing.T, root string) (config.Store, *credentials.MemoryStore, string) {
	t.Helper()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	secrets := credentials.NewMemoryStore()
	generation := "generation-1"
	owner := "owner-generation-1"
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		PendingRevocations: map[string]config.PendingRevocation{generation: {
			Generation: generation, CleanupRequested: true, DockerRestored: true, LocalCleaned: true,
			Device: config.Device{ClientDeviceID: "windows", PairingGeneration: generation, RevocationCredentialOwner: owner},
		}},
	}); err != nil {
		t.Fatalf("seed pending cleanup: %v", err)
	}
	if err := secrets.Put(owner, revocationProofCredential, bytes.Repeat([]byte{3}, pairing.RevocationProofSize)); err != nil {
		t.Fatalf("seed cleanup proof: %v", err)
	}
	return store, secrets, generation
}

type blockingRevokeTransport struct {
	calls        atomic.Int32
	started      chan struct{}
	release      chan struct{}
	once         sync.Once
	beforeRevoke func()
}

func (t *blockingRevokeTransport) Candidates(context.Context) ([]pairingTarget, error) {
	return nil, nil
}
func (t *blockingRevokeTransport) Bootstrap(context.Context, string, ed25519.PublicKey) (pairingTarget, pairing.SessionDescriptor, error) {
	return pairingTarget{}, pairing.SessionDescriptor{}, errors.New("unused")
}
func (t *blockingRevokeTransport) Status(context.Context, pairingTarget, pairing.SessionDescriptor) (pairing.SessionStatus, error) {
	return pairing.SessionStatus{}, errors.New("unused")
}
func (t *blockingRevokeTransport) Cancel(context.Context, pairingTarget, pairing.SessionDescriptor) error {
	return errors.New("unused")
}
func (t *blockingRevokeTransport) Confirm(context.Context, pairingTarget, pairing.SessionDescriptor, string, string, string, string, []byte) (pairing.DeviceRecord, error) {
	return pairing.DeviceRecord{}, errors.New("unused")
}
func (t *blockingRevokeTransport) Revoke(ctx context.Context, _ config.Device, _, _ string, _ []byte) error {
	t.calls.Add(1)
	if t.beforeRevoke != nil {
		t.beforeRevoke()
	}
	if t.started != nil {
		t.once.Do(func() { close(t.started) })
	}
	if t.release == nil {
		if t.started != nil {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	select {
	case <-t.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type contextObservingDockerExecutor struct {
	managedContextExecutor
	contextErr error
	restored   bool
}

func (e *contextObservingDockerExecutor) Run(ctx context.Context, invocation dockercli.Invocation) error {
	e.contextErr = ctx.Err()
	if len(invocation.Args) >= 2 && (invocation.Args[1] == "rm" || invocation.Args[1] == "update") {
		e.restored = true
	}
	return e.managedContextExecutor.Run(ctx, invocation)
}

type unknownCreateResultExecutor struct {
	inspect   int
	createErr error
}

func (e *unknownCreateResultExecutor) Run(_ context.Context, invocation dockercli.Invocation) error {
	if len(invocation.Args) < 2 || invocation.Args[0] != "context" {
		return nil
	}
	switch invocation.Args[1] {
	case "inspect":
		e.inspect++
		if e.inspect <= 2 {
			return runtimeExitError{code: 1}
		}
		return runtimeExitError{code: 2}
	case "create":
		e.createErr = context.Canceled
		return e.createErr
	default:
		return nil
	}
}

type inspectFailureDockerExecutor struct{}

func (inspectFailureDockerExecutor) Run(context.Context, dockercli.Invocation) error {
	return runtimeExitError{code: 2}
}

func (l *trackingStateLocker) WithLock(_ context.Context, operation func() error) error {
	l.held = true
	l.entries++
	defer func() { l.held = false }()
	return operation()
}

type runtimePairingTransport struct {
	hostKey           string
	private           ed25519.PrivateKey
	revoked           string
	revokeGeneration  string
	revokeErr         error
	revokeErrors      []error
	revokeCalls       int
	status            pairing.SessionState
	cancelled         string
	cancelErr         error
	cancelErrors      []error
	cancelCalls       int
	cancelCtxErr      error
	bootstrapCalls    int
	bootstrapErr      error
	invalidDescriptor bool
	afterBootstrap    func()
	events            []string
	targets           []pairingTarget
	afterConfirm      func()
}

type deadlineBlockingPairingTransport struct {
	*runtimePairingTransport
	calls []string
}

func (t *deadlineBlockingPairingTransport) Revoke(ctx context.Context, device config.Device, clientDeviceID, generation string, proof []byte) error {
	t.calls = append(t.calls, clientDeviceID)
	if clientDeviceID == "slow" {
		<-ctx.Done()
		return ctx.Err()
	}
	return t.runtimePairingTransport.Revoke(ctx, device, clientDeviceID, generation, proof)
}

type blockingBootstrapPairingTransport struct {
	*runtimePairingTransport
	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
	calls   int
}

type blockingConfirmPairingTransport struct {
	*runtimePairingTransport
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type cancelWinsConfirmPairingTransport struct {
	*runtimePairingTransport
	confirmStarted chan struct{}
	releaseConfirm chan struct{}
	confirmOnce    sync.Once
	cancelled      atomic.Bool
}

type delayedConfirmAfterNoopRevokeTransport struct {
	*runtimePairingTransport
	confirmStarted   chan struct{}
	releaseConfirm   chan struct{}
	confirmDone      chan struct{}
	startOnce        sync.Once
	doneOnce         sync.Once
	mu               sync.Mutex
	trusted          bool
	revokeCalls      int
	revokeGeneration string
}

type offlineCancellationPairingTransport struct {
	*runtimePairingTransport
}

type blockingCancelPairingTransport struct {
	*runtimePairingTransport
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	cancelCalls atomic.Int32
}

func (t *blockingCancelPairingTransport) Cancel(ctx context.Context, _ pairingTarget, _ pairing.SessionDescriptor) error {
	t.cancelCalls.Add(1)
	t.startOnce.Do(func() { close(t.started) })
	select {
	case <-t.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *offlineCancellationPairingTransport) Cancel(ctx context.Context, _ pairingTarget, _ pairing.SessionDescriptor) error {
	<-ctx.Done()
	return ctx.Err()
}

func (t *offlineCancellationPairingTransport) Revoke(
	ctx context.Context,
	_ config.Device,
	_, _ string,
	_ []byte,
) error {
	<-ctx.Done()
	return ctx.Err()
}

func (t *blockingConfirmPairingTransport) Confirm(
	ctx context.Context,
	target pairingTarget,
	descriptor pairing.SessionDescriptor,
	clientDeviceID, generation, authorizedKey, code string,
	revocationProof []byte,
) (pairing.DeviceRecord, error) {
	t.once.Do(func() { close(t.started) })
	select {
	case <-t.release:
		return t.runtimePairingTransport.Confirm(
			ctx, target, descriptor, clientDeviceID, generation, authorizedKey, code, revocationProof,
		)
	case <-ctx.Done():
		return pairing.DeviceRecord{}, ctx.Err()
	}
}

func (t *cancelWinsConfirmPairingTransport) Confirm(
	ctx context.Context,
	target pairingTarget,
	descriptor pairing.SessionDescriptor,
	clientDeviceID, generation, authorizedKey, code string,
	revocationProof []byte,
) (pairing.DeviceRecord, error) {
	t.confirmOnce.Do(func() { close(t.confirmStarted) })
	select {
	case <-t.releaseConfirm:
		if t.cancelled.Load() {
			return pairing.DeviceRecord{}, pairing.ErrSessionState
		}
		return t.runtimePairingTransport.Confirm(
			ctx, target, descriptor, clientDeviceID, generation, authorizedKey, code, revocationProof,
		)
	case <-ctx.Done():
		return pairing.DeviceRecord{}, ctx.Err()
	}
}

func (t *cancelWinsConfirmPairingTransport) Cancel(
	ctx context.Context,
	target pairingTarget,
	descriptor pairing.SessionDescriptor,
) error {
	t.cancelled.Store(true)
	return t.runtimePairingTransport.Cancel(ctx, target, descriptor)
}

func (t *delayedConfirmAfterNoopRevokeTransport) Confirm(
	ctx context.Context,
	target pairingTarget,
	descriptor pairing.SessionDescriptor,
	clientDeviceID, generation, authorizedKey, code string,
	revocationProof []byte,
) (pairing.DeviceRecord, error) {
	t.startOnce.Do(func() { close(t.confirmStarted) })
	<-t.releaseConfirm
	record, err := t.runtimePairingTransport.Confirm(
		ctx, target, descriptor, clientDeviceID, generation, authorizedKey, code, revocationProof,
	)
	if err == nil {
		t.mu.Lock()
		t.trusted = true
		t.mu.Unlock()
	}
	t.doneOnce.Do(func() { close(t.confirmDone) })
	return record, err
}

func (t *delayedConfirmAfterNoopRevokeTransport) Cancel(ctx context.Context, _ pairingTarget, _ pairing.SessionDescriptor) error {
	<-ctx.Done()
	return ctx.Err()
}

func (t *delayedConfirmAfterNoopRevokeTransport) Status(ctx context.Context, _ pairingTarget, descriptor pairing.SessionDescriptor) (pairing.SessionStatus, error) {
	select {
	case <-t.confirmStarted:
		select {
		case <-t.confirmDone:
			return pairing.SessionStatus{SessionID: descriptor.ID, State: pairing.SessionCompleted, ExpiresAt: descriptor.ExpiresAt}, nil
		case <-ctx.Done():
			return pairing.SessionStatus{}, ctx.Err()
		}
	default:
		return pairing.SessionStatus{SessionID: descriptor.ID, State: pairing.SessionApproved, ExpiresAt: descriptor.ExpiresAt}, nil
	}
}

func (t *delayedConfirmAfterNoopRevokeTransport) Revoke(_ context.Context, _ config.Device, _, generation string, _ []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.revokeCalls++
	t.revokeGeneration = generation
	if t.trusted {
		t.trusted = false
	}
	return nil
}

func (t *delayedConfirmAfterNoopRevokeTransport) Trusted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.trusted
}

func (t *delayedConfirmAfterNoopRevokeTransport) RevokeCalls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.revokeCalls
}

func (t *delayedConfirmAfterNoopRevokeTransport) LastRevokeGeneration() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.revokeGeneration
}

func (t *blockingBootstrapPairingTransport) Bootstrap(ctx context.Context, selector string, key ed25519.PublicKey) (pairingTarget, pairing.SessionDescriptor, error) {
	t.mu.Lock()
	t.calls++
	if t.calls == 1 {
		close(t.started)
	}
	t.mu.Unlock()
	<-t.release
	return t.runtimePairingTransport.Bootstrap(ctx, selector, key)
}

func (t *blockingBootstrapPairingTransport) BootstrapCalls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

type managedContextExecutor struct {
	mu          sync.Mutex
	host        string
	description string
	commands    [][]string
}

type contextAppearsAfterPlanExecutor struct {
	foreignHost       string
	host              string
	inspect           int
	createCalls       int
	dangerousMutation bool
	commands          [][]string
}

type contextCreatedBeforeApplyExecutor struct {
	host        string
	description string
	inspect     int
}

func (e *contextCreatedBeforeApplyExecutor) Run(_ context.Context, invocation dockercli.Invocation) error {
	if len(invocation.Args) < 2 || invocation.Args[0] != "context" || invocation.Args[1] != "inspect" {
		return nil
	}
	e.inspect++
	if e.inspect == 1 {
		return runtimeExitError{code: 1}
	}
	_, _ = fmt.Fprintf(invocation.Stdout, `[{"Name":"remote-docker","Metadata":{"Description":%q},"Endpoints":{"docker":{"Host":%q}}}]`, e.description, e.host)
	return nil
}

func (e *contextAppearsAfterPlanExecutor) Run(_ context.Context, invocation dockercli.Invocation) error {
	e.commands = append(e.commands, append([]string(nil), invocation.Args...))
	if len(invocation.Args) < 2 || invocation.Args[0] != "context" {
		return nil
	}
	if invocation.Args[1] == "inspect" {
		e.inspect++
		if e.inspect <= 2 {
			return runtimeExitError{code: 1}
		}
		_, _ = fmt.Fprintf(invocation.Stdout, `[{"Name":"remote-docker","Metadata":{"Description":"Managed by Remote Docker"},"Endpoints":{"docker":{"Host":%q}}}]`, e.host)
		return nil
	}
	if invocation.Args[1] == "create" {
		e.createCalls++
		e.host = e.foreignHost
		return runtimeExitError{code: 1}
	}
	e.dangerousMutation = true
	return nil
}

func (e *managedContextExecutor) Run(_ context.Context, invocation dockercli.Invocation) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.commands = append(e.commands, append([]string(nil), invocation.Args...))
	if len(invocation.Args) >= 2 && invocation.Args[0] == "context" && invocation.Args[1] == "inspect" {
		if e.host == "" {
			return runtimeExitError{code: 1}
		}
		description := e.description
		if description == "" {
			description = "Managed by Remote Docker"
		}
		_, _ = fmt.Fprintf(invocation.Stdout, `[{"Name":"remote-docker","Metadata":{"Description":%q},"Endpoints":{"docker":{"Host":%q}}}]`, description, e.host)
	}
	if len(invocation.Args) >= 5 && invocation.Args[0] == "context" && invocation.Args[1] == "update" {
		for index, argument := range invocation.Args {
			if argument == "--description" && index+1 < len(invocation.Args) {
				e.description = invocation.Args[index+1]
			}
			if strings.HasPrefix(argument, "host=") {
				e.host = strings.TrimPrefix(argument, "host=")
			}
		}
	}
	if len(invocation.Args) >= 2 && invocation.Args[0] == "context" && invocation.Args[1] == "rm" {
		e.host = ""
		e.description = ""
	}
	if len(invocation.Args) >= 2 && invocation.Args[0] == "context" && invocation.Args[1] == "create" {
		for index, argument := range invocation.Args {
			if argument == "--description" && index+1 < len(invocation.Args) {
				e.description = invocation.Args[index+1]
			}
			if strings.HasPrefix(argument, "host=") {
				e.host = strings.TrimPrefix(argument, "host=")
			}
		}
	}
	return nil
}

func (e *managedContextExecutor) Commands() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	commands := make([][]string, len(e.commands))
	for index := range e.commands {
		commands[index] = append([]string(nil), e.commands[index]...)
	}
	return commands
}

func (t *runtimePairingTransport) Candidates(context.Context) ([]pairingTarget, error) {
	if t.targets != nil {
		return append([]pairingTarget(nil), t.targets...), nil
	}
	return []pairingTarget{{Name: "Dev PC", InstanceID: "windows-peer", Address: "192.168.1.20", PairingPort: 43119}}, nil
}

func (t *runtimePairingTransport) Bootstrap(_ context.Context, _ string, clientPublicKey ed25519.PublicKey) (pairingTarget, pairing.SessionDescriptor, error) {
	t.bootstrapCalls++
	t.events = append(t.events, "bootstrap")
	if t.bootstrapErr != nil {
		return pairingTarget{}, pairing.SessionDescriptor{}, t.bootstrapErr
	}
	serverPublic, serverPrivate, _ := ed25519.GenerateKey(nil)
	t.private = serverPrivate
	descriptor := pairing.SessionDescriptor{
		ID: "session-1", Nonce: []byte("01234567890123456789012345678901"),
		ServerPublicKey: serverPublic, ClientPublicKey: append(ed25519.PublicKey(nil), clientPublicKey...),
		ExpiresAt: time.Now().Add(time.Minute),
	}
	if t.invalidDescriptor {
		descriptor.Nonce = nil
	}
	if t.afterBootstrap != nil {
		t.afterBootstrap()
		t.afterBootstrap = nil
	}
	return pairingTarget{Name: "Dev PC", Address: "192.168.1.20", PairingPort: 43119}, descriptor, nil
}

func (t *runtimePairingTransport) Confirm(_ context.Context, _ pairingTarget, descriptor pairing.SessionDescriptor, clientDeviceID, generation, authorizedKey, code string, revocationProof []byte) (pairing.DeviceRecord, error) {
	want, _ := pairing.Code(descriptor)
	if code != want || clientDeviceID == "" || generation == "" || !strings.HasPrefix(authorizedKey, "ssh-ed25519 ") || len(revocationProof) != pairing.RevocationProofSize {
		return pairing.DeviceRecord{}, errors.New("invalid confirmation")
	}
	record := pairing.DeviceRecord{
		DeviceID: clientDeviceID, AuthorizedKeys: []string{authorizedKey},
		SSHHostPublicKey: t.hostKey, SyncthingDeviceID: "WINDOWS-SYNC",
		SSHPort: 49222, SyncthingPort: 49220,
		TunnelPublicKey: append(ed25519.PublicKey(nil), descriptor.ServerPublicKey...),
		TunnelPort:      tunnel.TunnelPort, TransportVersion: tunnel.CurrentTransportVersion,
	}
	if t.afterConfirm != nil {
		t.afterConfirm()
		t.afterConfirm = nil
	}
	return record, nil
}

func (t *runtimePairingTransport) Status(_ context.Context, _ pairingTarget, descriptor pairing.SessionDescriptor) (pairing.SessionStatus, error) {
	state := t.status
	if state == "" {
		state = pairing.SessionApproved
	}
	return pairing.SessionStatus{SessionID: descriptor.ID, State: state, ExpiresAt: descriptor.ExpiresAt}, nil
}

func (t *runtimePairingTransport) Cancel(ctx context.Context, _ pairingTarget, descriptor pairing.SessionDescriptor) error {
	t.cancelCalls++
	t.events = append(t.events, "cancel")
	t.cancelled = descriptor.ID
	t.cancelCtxErr = ctx.Err()
	if t.cancelCalls <= len(t.cancelErrors) {
		return t.cancelErrors[t.cancelCalls-1]
	}
	return t.cancelErr
}

func (t *runtimePairingTransport) Revoke(_ context.Context, _ config.Device, clientDeviceID, generation string, proof []byte) error {
	t.revokeCalls++
	t.revoked = clientDeviceID
	t.revokeGeneration = generation
	if generation == "" || len(proof) != pairing.RevocationProofSize {
		return errors.New("invalid revocation proof")
	}
	if t.revokeCalls <= len(t.revokeErrors) {
		return t.revokeErrors[t.revokeCalls-1]
	}
	return t.revokeErr
}

type runtimeDockerExecutor struct {
	calls       [][]string
	host        string
	description string
}

func (e *runtimeDockerExecutor) Run(_ context.Context, invocation dockercli.Invocation) error {
	e.calls = append(e.calls, append([]string(nil), invocation.Args...))
	if len(invocation.Args) < 2 || invocation.Args[0] != "context" {
		return nil
	}
	switch invocation.Args[1] {
	case "inspect":
		if e.host == "" {
			return runtimeExitError{code: 1}
		}
		_, _ = fmt.Fprintf(invocation.Stdout, `[{"Name":"remote-docker","Metadata":{"Description":%q},"Endpoints":{"docker":{"Host":%q}}}]`, e.description, e.host)
	case "create", "update":
		for index, argument := range invocation.Args {
			if argument == "--description" && index+1 < len(invocation.Args) {
				e.description = invocation.Args[index+1]
			}
			if strings.HasPrefix(argument, "host=") {
				e.host = strings.TrimPrefix(argument, "host=")
			}
		}
	case "rm":
		e.host = ""
		e.description = ""
	}
	return nil
}

type dockerIntentCheckingExecutor struct {
	beforeMutation func() error
	mutated        bool
	host           string
	description    string
}

func (e *dockerIntentCheckingExecutor) Run(_ context.Context, invocation dockercli.Invocation) error {
	if len(invocation.Args) >= 2 && invocation.Args[0] == "context" && invocation.Args[1] == "inspect" {
		if e.mutated {
			_, _ = fmt.Fprintf(invocation.Stdout, `[{"Name":"remote-docker","Metadata":{"Description":%q},"Endpoints":{"docker":{"Host":%q}}}]`, e.description, e.host)
			return nil
		}
		return runtimeExitError{code: 1}
	}
	if len(invocation.Args) >= 2 && invocation.Args[0] == "context" && invocation.Args[1] == "create" {
		if err := e.beforeMutation(); err != nil {
			return err
		}
		for index, argument := range invocation.Args {
			if argument == "--description" && index+1 < len(invocation.Args) {
				e.description = invocation.Args[index+1]
			}
			if strings.HasPrefix(argument, "host=") {
				e.host = strings.TrimPrefix(argument, "host=")
			}
		}
		e.mutated = true
	}
	return nil
}

type contextCheckingDockerExecutor struct{}

func (contextCheckingDockerExecutor) Run(ctx context.Context, _ dockercli.Invocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return runtimeExitError{code: 1}
}

type runtimeExitError struct{ code int }

func (e runtimeExitError) Error() string { return "exit" }
func (e runtimeExitError) ExitCode() int { return e.code }

func testAuthorizedKey(t *testing.T) string {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	key, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatalf("NewPublicKey() error = %v", err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

func TestInfrastructureRestorerReplacesLongRunningEventStreamAndReconcilesRelays(t *testing.T) {
	source := &runtimeRelaySource{eventsStarted: make(chan struct{}, 4)}
	sink := &runtimeRelaySink{}
	restorer := newInfrastructureRestorer(func(context.Context) (portrelay.Reconciler, error) {
		return portrelay.Reconciler{Source: source, Sink: sink, MinBackoff: time.Hour, MaxBackoff: time.Hour}, nil
	})
	lifecycle, stop := context.WithCancel(context.Background())
	defer stop()
	restorer.Bind(lifecycle)
	agent := NewAgent(ObservationFunc(func(context.Context) AgentObservation {
		return AgentObservation{Paired: true, PinnedSSH: true, DockerPing: true, SyncthingConnected: true}
	}), restorer, nil)

	if err := agent.Reconnect(context.Background()); err != nil {
		t.Fatalf("first Reconnect() error = %v", err)
	}
	select {
	case <-source.eventsStarted:
	case <-time.After(time.Second):
		t.Fatal("first Docker event stream did not start")
	}
	if err := agent.Reconnect(context.Background()); err != nil {
		t.Fatalf("second Reconnect() error = %v", err)
	}
	select {
	case <-source.eventsStarted:
	case <-time.After(time.Second):
		t.Fatal("replacement Docker event stream did not start")
	}
	if sink.Calls() < 2 {
		t.Fatalf("relay reconciles = %d, want at least 2", sink.Calls())
	}
}

func TestInfrastructureRestorerStopJoinsEveryReplacedEventStream(t *testing.T) {
	source := &joiningRelaySource{
		started:   make(chan struct{}, 2),
		cancelled: make(chan struct{}, 2),
		release:   make(chan struct{}, 2),
	}
	restorer := newInfrastructureRestorer(func(context.Context) (portrelay.Reconciler, error) {
		return portrelay.Reconciler{Source: source, Sink: &runtimeRelaySink{}, MinBackoff: time.Hour, MaxBackoff: time.Hour}, nil
	})
	restorer.Bind(context.Background())
	for attempt := 0; attempt < 2; attempt++ {
		if err := restorer.RestoreEventStream(context.Background()); err != nil {
			t.Fatalf("RestoreEventStream(%d) error = %v", attempt+1, err)
		}
		waitForTestSignal(t, source.started, "Docker event stream start")
	}
	stopDone := make(chan struct{})
	go func() {
		restorer.Stop()
		close(stopDone)
	}()
	waitForTestSignal(t, source.cancelled, "first Docker event stream cancellation")
	waitForTestSignal(t, source.cancelled, "second Docker event stream cancellation")
	select {
	case <-stopDone:
		t.Fatal("restorer Stop returned before replaced event streams exited")
	default:
	}
	source.release <- struct{}{}
	source.release <- struct{}{}
	waitForTestSignal(t, stopDone, "restorer event stream join")
}

type joiningRelaySource struct {
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
}

func (*joiningRelaySource) RunningContainers(context.Context) ([]portrelay.Container, error) {
	return []portrelay.Container{{ID: "running", Running: true}}, nil
}

func (s *joiningRelaySource) Events(ctx context.Context) (<-chan portrelay.Event, error) {
	stream := make(chan portrelay.Event)
	s.started <- struct{}{}
	go func() {
		<-ctx.Done()
		s.cancelled <- struct{}{}
		<-s.release
		close(stream)
	}()
	return stream, nil
}

type runtimeRelaySource struct{ eventsStarted chan struct{} }

func (*runtimeRelaySource) RunningContainers(context.Context) ([]portrelay.Container, error) {
	return []portrelay.Container{{ID: "running", Running: true}}, nil
}

func (s *runtimeRelaySource) Events(ctx context.Context) (<-chan portrelay.Event, error) {
	stream := make(chan portrelay.Event)
	s.eventsStarted <- struct{}{}
	go func() {
		defer close(stream)
		<-ctx.Done()
	}()
	return stream, nil
}

type runtimeRelaySink struct {
	calls atomic.Int32
}

func (s *runtimeRelaySink) Apply(context.Context, portrelay.Snapshot) error {
	s.calls.Add(1)
	return nil
}

func (s *runtimeRelaySink) Calls() int {
	return int(s.calls.Load())
}

func TestWindowsPairingHostRetriesAndPublishesOnFirewallApprovedLANPort(t *testing.T) {
	host, err := newWindowsPairingHost(&runtimePairingInstaller{})
	if err != nil {
		t.Fatalf("newWindowsPairingHost() error = %v", err)
	}
	publisher := &retryingPairingPublisher{published: make(chan discovery.Advertisement, 1)}
	host.publisher = publisher
	host.minRetryBackoff = time.Millisecond
	host.maxRetryBackoff = 2 * time.Millisecond
	host.republishInterval = time.Hour
	var listenAddress string
	var listenerPort atomic.Int32
	var listenCalls atomic.Int32
	host.listen = func(_ string, address string) (net.Listener, error) {
		listenAddress = address
		if listenCalls.Add(1) == 1 {
			return nil, errors.New("private LAN is not available yet")
		}
		listenerPort.Store(43119)
		return newBlockingPairingListener(43119), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		host.Run(ctx)
		close(done)
	}()

	var advertisement discovery.Advertisement
	select {
	case advertisement = <-publisher.published:
	case <-time.After(time.Second):
		cancel()
		t.Fatalf(
			"pairing host did not retry the temporary publication failure: listens=%d publishes=%d port=%d",
			listenCalls.Load(), publisher.calls.Load(), listenerPort.Load(),
		)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pairing host did not stop after cancellation")
	}

	if listenAddress != ":49221" {
		t.Fatalf("pairing listen address = %q, want firewall-approved port", listenAddress)
	}
	if publisher.calls.Load() != 2 {
		t.Fatalf("publish calls = %d, want one failure and one retry", publisher.calls.Load())
	}
	if listenCalls.Load() != 2 {
		t.Fatalf("listen calls = %d, want no-network failure and retry", listenCalls.Load())
	}
	if advertisement.Port != int(listenerPort.Load()) {
		t.Fatalf("published port = %d, want reachable listener port %d", advertisement.Port, listenerPort.Load())
	}
	if want := []string{"version=1", "instance=" + host.server.InstanceID(), "pairing=1"}; !reflect.DeepEqual(advertisement.TXT, want) {
		t.Fatalf("advertisement TXT = %#v, want opaque TLS-bound data %#v", advertisement.TXT, want)
	}
}

func TestWindowsPairingStatusTreatsNoActiveSessionAsIdle(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	server, err := pairing.NewServer(pairing.ServerIdentity{PrivateKey: privateKey})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	status, err := (windowsPairingCoordinator{server: server}).Status(context.Background(), "")
	if err != nil || status.SessionID != "" || status.Status != "" {
		t.Fatalf("idle pairing status = %#v error=%v", status, err)
	}
}

func TestWindowsPairingCoordinatorCancelsPendingAndApprovedExactSession(t *testing.T) {
	for _, state := range []pairing.SessionState{pairing.SessionPending, pairing.SessionApproved} {
		t.Run(string(state), func(t *testing.T) {
			_, privateKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			server, err := pairing.NewServer(pairing.ServerIdentity{PrivateKey: privateKey})
			if err != nil {
				t.Fatal(err)
			}
			clientPublicKey, _, _ := ed25519.GenerateKey(rand.Reader)
			descriptor, err := server.StartSession(clientPublicKey, pairing.MaxSessionTTL)
			if err != nil {
				t.Fatal(err)
			}
			if state == pairing.SessionApproved {
				if err := server.Approve(descriptor.ID); err != nil {
					t.Fatal(err)
				}
			}

			result, err := (windowsPairingCoordinator{server: server}).Cancel(context.Background(), descriptor.ID)
			if err != nil || result.SessionID != descriptor.ID || result.Status != string(pairing.SessionCancelled) {
				t.Fatalf("Cancel() = %#v error=%v", result, err)
			}
			if _, _, active := server.ActiveSession(); active {
				t.Fatal("cancelled Windows pairing still exposes comparison code")
			}
		})
	}
}

func TestProductionAgentControllerBindsTrustRevokedObserverToPairingCoordinator(t *testing.T) {
	coordinator := &trustRevokedBindingCoordinator{}
	controller := &productionAgentController{pairing: coordinator}
	want := trustRevokedObserver(func(context.Context, string, string) error { return nil })

	controller.bindTrustRevokedObserver(want)

	if coordinator.observer == nil {
		t.Fatal("trust-revoked observer was not forwarded to pairing coordinator")
	}
}

func TestProductionAgentControllerCachesTerminalCancellationForExactSession(t *testing.T) {
	coordinator := &countingPairingCancelCoordinator{}
	controller := &productionAgentController{pairing: coordinator}
	raw, _ := json.Marshal(localapi.PairSessionParams{SessionID: "session-one"})

	first, err := controller.Handle(context.Background(), localapi.MethodPairCancel, raw)
	if err != nil {
		t.Fatalf("PairCancel error = %v", err)
	}
	second, err := controller.cancelCurrentPairing(context.Background(), "session-one")
	if err != nil {
		t.Fatalf("cancelCurrentPairing error = %v", err)
	}
	if coordinator.calls != 1 {
		t.Fatalf("coordinator Cancel calls = %d, want 1", coordinator.calls)
	}
	if first != second {
		t.Fatalf("cached cancellation = %#v, want %#v", second, first)
	}

	if _, err := controller.cancelCurrentPairing(context.Background(), "session-two"); err != nil {
		t.Fatalf("different-session cancelCurrentPairing error = %v", err)
	}
	if coordinator.calls != 2 {
		t.Fatalf("coordinator Cancel calls after different session = %d, want 2", coordinator.calls)
	}
}

func TestWindowsProofRevokeClearsExactLifecycleTrustAfterDurableSave(t *testing.T) {
	store := config.Store{Path: filepath.Join(t.TempDir(), "config.json")}
	var saveCalls atomic.Int32
	registry := windowsPairingRegistry{
		store: store,
		saveConfig: func(cfg config.Config) error {
			saveCalls.Add(1)
			return store.Save(cfg)
		},
	}
	installer := &runtimePairingInstaller{device: pairing.DeviceInfo{
		SSHHostPublicKey: "ssh-ed25519 WINDOWS-HOST", SyncthingDeviceID: "WINDOWS-SYNC",
		SSHPort: 49222, SyncthingPort: 49220,
	}}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := tunnel.Identity{PrivateKey: privateKey, PublicKey: publicKey}
	host, err := newWindowsPairingHostWithRegistryAndIdentity(installer, registry, identity)
	if err != nil {
		t.Fatalf("newWindowsPairingHostWithRegistryAndIdentity() error = %v", err)
	}
	coordinator := &windowsPairingCoordinator{server: host.server, installer: installer, registry: &registry}
	agent := NewAgent(nil, nil, &productionAgentController{pairing: coordinator})
	machine := newLifecycleMachine(t, lifecycle.RoleWindowsHost)
	runtime := newRecordingSessionRuntime()
	stopStarted := make(chan struct{})
	releaseStop := make(chan struct{})
	runtime.onStop = func() {
		close(stopStarted)
		<-releaseStop
	}
	supervisor, _ := NewSupervisor(machine, runtime)
	controller, err := NewDesktopController(supervisor, agent)
	if err != nil {
		t.Fatalf("NewDesktopController() error = %v", err)
	}
	if _, err := controller.Handle(context.Background(), localapi.MethodEnable, nil); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	clientPublicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := host.server.StartSession(clientPublicKey, pairing.MaxSessionTTL)
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	code, _ := pairing.Code(descriptor)
	pairingState := lifecycle.Pairing{
		SessionID: descriptor.ID, Peer: lifecycle.Peer{ID: pairing.InstanceIDFromPublicKey(clientPublicKey), Name: "Mac"},
		Code: code, ExpiresAt: descriptor.ExpiresAt,
	}
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingStarted, Pairing: &pairingState})
	if err := host.server.Approve(descriptor.ID); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingApproved})

	tlsConfig, err := host.server.TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig() error = %v", err)
	}
	certificate, err := tlsConfig.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate() error = %v", err)
	}
	tlsConfig = tlsConfig.Clone()
	tlsConfig.GetCertificate = nil
	tlsConfig.Certificates = []tls.Certificate{*certificate}
	httpServer := httptest.NewUnstartedServer(host.server)
	httpServer.TLS = tlsConfig
	httpServer.StartTLS()
	defer httpServer.Close()
	proof := bytes.Repeat([]byte{7}, pairing.RevocationProofSize)
	client := pairing.Client{
		BaseURL: httpServer.URL, Session: descriptor, HTTPClient: pairing.NewPinnedHTTPClient(identity.PublicKey, nil),
		DeviceID: "mac-one", Generation: "generation-one", AuthorizedKey: "ssh-ed25519 MAC-KEY", RevocationProof: proof,
	}
	if _, _, err := client.Confirm(context.Background(), code); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	_, _ = machine.Apply(lifecycle.Event{Type: lifecycle.EventPairingCompleted, Peer: &lifecycle.Peer{
		ID: "mac-one", Name: "Mac", Generation: "generation-one",
	}})
	completed, err := coordinator.Cancel(context.Background(), descriptor.ID)
	if err != nil || completed.Status != string(pairing.SessionCompleted) || completed.Device == nil || completed.Device.ID != "mac-one" {
		t.Fatalf("Cancel() after durable completion = %#v error=%v", completed, err)
	}
	committed, err := store.Load()
	if err != nil || committed.ActiveDevice != "mac-one" || committed.Devices["mac-one"].PairingGeneration != "generation-one" || installer.revokes != 0 {
		t.Fatalf("completed cancel changed durable trust = %#v revokes=%d error=%v", committed, installer.revokes, err)
	}
	cancelDone := make(chan error, 1)
	go func() {
		_, cancelErr := controller.Handle(context.Background(), localapi.MethodConnectionCancel, nil)
		cancelDone <- cancelErr
	}()
	waitForTestSignal(t, stopStarted, "connection cancellation stop")
	if got := machine.Snapshot(); got.State != lifecycle.StateStopping || got.TrustedPeers != 1 || got.Peer == nil || got.Peer.ID != "mac-one" {
		t.Fatalf("lifecycle while stop is blocked = %#v", got)
	}

	if err := client.Revoke(context.Background(), "mac-one", "generation-one", proof); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	cfg, err := store.Load()
	if err != nil || cfg.ActiveDevice != "" || len(cfg.Devices) != 0 {
		t.Fatalf("durable registry after revoke = %#v error=%v", cfg, err)
	}
	if _, _, pending := host.server.CompletedTrust(descriptor.ID); !pending {
		t.Fatal("exact completed ownership was discarded before lifecycle acknowledgement")
	}
	if got := machine.Snapshot(); got.State != lifecycle.StateStopping || got.TrustedPeers != 1 || got.Peer == nil {
		t.Fatalf("blocked lifecycle was changed before StopCompleted = %#v", got)
	}
	close(releaseStop)
	if err := waitForTestError(t, cancelDone, "connection cancellation result"); err != nil {
		t.Fatalf("ConnectionCancel() error = %v", err)
	}
	got := machine.Snapshot()
	if got.State != lifecycle.StateHostWaiting || got.TrustedPeers != 0 || got.Peer != nil || got.Pairing != nil ||
		installer.revokes != 1 || saveCalls.Load() != 2 {
		t.Fatalf("lifecycle after durable revoke = %#v installer revokes=%d saves=%d", got, installer.revokes, saveCalls.Load())
	}
	if _, _, pending := host.server.CompletedTrust(descriptor.ID); pending {
		t.Fatal("acknowledged completed ownership was not retired")
	}
}

func TestWindowsProofRevokeClearsRestoredLifecycleTrustAfterRestart(t *testing.T) {
	store := config.Store{Path: filepath.Join(t.TempDir(), "config.json")}
	var saveCalls atomic.Int32
	registry := windowsPairingRegistry{
		store: store,
		saveConfig: func(cfg config.Config) error {
			saveCalls.Add(1)
			return store.Save(cfg)
		},
	}
	proof := bytes.Repeat([]byte{7}, pairing.RevocationProofSize)
	peerKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Commit(context.Background(), pairing.TrustedPeer{
		DeviceID: "mac-one", Generation: "generation-one", PublicKey: peerKey,
		RevocationProofHash: sha256.Sum256(proof),
	}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	installer := &runtimePairingInstaller{}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := tunnel.Identity{PrivateKey: privateKey, PublicKey: publicKey}
	host, err := newWindowsPairingHostWithRegistryAndIdentity(installer, registry, identity)
	if err != nil {
		t.Fatalf("newWindowsPairingHostWithRegistryAndIdentity() error = %v", err)
	}
	coordinator := &windowsPairingCoordinator{server: host.server, installer: installer, registry: &registry}
	agent := NewAgent(nil, nil, &productionAgentController{pairing: coordinator})
	restored, err := lifecycle.NewMachine(
		lifecycle.RoleWindowsHost,
		"Windows",
		lifecycle.WithTrustedPeer(lifecycle.Peer{
			ID: "mac-one", Name: "Mac", Generation: registry.committedGeneration("mac-one"),
		}),
	)
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	supervisor, _ := NewSupervisor(restored, newRecordingSessionRuntime())
	controller, err := NewDesktopController(supervisor, agent)
	if err != nil {
		t.Fatalf("NewDesktopController() error = %v", err)
	}

	if err := registry.RevokeWithProof(
		context.Background(), installer, "mac-one", "generation-one", proof,
		func(ctx context.Context, deviceID, generation string) error {
			return host.server.PublishTrustRevoked(ctx, deviceID, generation)
		},
	); err != nil {
		t.Fatalf("RevokeWithProof() error = %v", err)
	}
	got := restored.Snapshot()
	if got.State != lifecycle.StatePaused || got.TrustedPeers != 0 || got.Peer != nil ||
		controller.pendingTrustRevoke != nil || installer.revokes != 1 || saveCalls.Load() != 2 {
		t.Fatalf("restored lifecycle after revoke = %#v pending=%#v revokes=%d saves=%d", got, controller.pendingTrustRevoke, installer.revokes, saveCalls.Load())
	}
	if err := host.server.RetryTrustRevoked(context.Background(), "mac-one", "generation-one"); err != nil {
		t.Fatalf("acknowledged RetryTrustRevoked() error = %v", err)
	}
	if installer.revokes != 1 || saveCalls.Load() != 2 {
		t.Fatalf("local retry repeated durable effects: revokes=%d saves=%d", installer.revokes, saveCalls.Load())
	}
}

func TestPrivatePeerListenerRejectsPublicPeerAtAcceptBoundary(t *testing.T) {
	public := &addressedConn{remote: &net.TCPAddr{IP: net.ParseIP("203.0.113.10"), Port: 43119}}
	private := &addressedConn{remote: &net.TCPAddr{IP: net.ParseIP("192.168.1.20"), Port: 43119}}
	listener := &queuedListener{connections: []net.Conn{public, private}}

	accepted, err := (privatePeerListener{Listener: listener}).Accept()
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	if accepted != private {
		t.Fatalf("Accept() returned %v, want private peer", accepted.RemoteAddr())
	}
	if public.closes.Load() != 1 {
		t.Fatalf("public peer closes = %d, want 1", public.closes.Load())
	}
}

func TestManagedSSHRuntimeEnsureRestartsDeadAgentForReconnect(t *testing.T) {
	root := t.TempDir()
	store := config.Store{Path: filepath.Join(root, "config.json")}
	if err := store.Save(config.Config{
		SchemaVersion: config.CurrentSchemaVersion,
		ActiveDevice:  "pc-1",
		Devices: map[string]config.Device{
			"pc-1": {Address: "192.168.1.20", SSHPort: 49222},
		},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	secrets := credentials.NewMemoryStore()
	if err := secrets.Put("pc-1", sshtransport.SSHPrivateKeyCredential, []byte("private-key")); err != nil {
		t.Fatalf("seed private key: %v", err)
	}

	var agents []*recordingManagedSSHAgent
	sshRuntime := &managedSSHRuntime{
		store: store, secrets: secrets,
		sshConfigPath:   filepath.Join(root, "ssh_config"),
		knownHostsPath:  filepath.Join(root, "known_hosts"),
		agentSocketPath: filepath.Join(root, "agent", "ssh-agent.sock"),
		controlDir:      filepath.Join(root, "control"),
		start: func(context.Context, string, []byte) (managedSSHAgent, error) {
			agent := &recordingManagedSSHAgent{}
			agents = append(agents, agent)
			return agent, nil
		},
		probe: func(context.Context, string) error {
			return errors.New("managed ssh-agent socket is stale")
		},
	}

	if err := sshRuntime.Ensure(context.Background()); err != nil {
		t.Fatalf("first Ensure() error = %v", err)
	}
	if err := sshRuntime.Ensure(context.Background()); err != nil {
		t.Fatalf("reconnect Ensure() error = %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("managed agent starts = %d, want 2", len(agents))
	}
	if agents[0].closes.Load() != 1 {
		t.Fatalf("dead managed agent closes = %d, want 1", agents[0].closes.Load())
	}
}

type runtimePairingInstaller struct {
	device  pairing.DeviceInfo
	revokes int
}

type generationPairingHandler struct {
	result     localapi.PairingStatusResult
	generation string
}

func (h *generationPairingHandler) Handle(context.Context, localapi.Method, json.RawMessage) (any, error) {
	return h.result, nil
}

func (h *generationPairingHandler) committedPairingGeneration(string) string {
	return h.generation
}

type trustRevokedBindingCoordinator struct {
	runtimePairingCoordinator
	observer trustRevokedObserver
}

func (c *trustRevokedBindingCoordinator) bindTrustRevokedObserver(observer trustRevokedObserver) {
	c.observer = observer
}

type countingPairingCancelCoordinator struct {
	runtimePairingCoordinator
	calls int
}

func (c *countingPairingCancelCoordinator) Cancel(_ context.Context, sessionID string) (localapi.PairingStatusResult, error) {
	c.calls++
	return localapi.PairingStatusResult{SessionID: sessionID, Status: string(pairing.SessionCancelled)}, nil
}

func (i *runtimePairingInstaller) Install(context.Context, string, string) (pairing.DeviceInfo, error) {
	return i.device, nil
}

func (i *runtimePairingInstaller) Revoke(context.Context, string) error {
	i.revokes++
	return nil
}

type retryingPairingPublisher struct {
	calls     atomic.Int32
	published chan discovery.Advertisement
}

func (p *retryingPairingPublisher) Publish(_ context.Context, advertisement discovery.Advertisement) (discovery.Registration, error) {
	if p.calls.Add(1) == 1 {
		return nil, errors.New("temporary mDNS failure")
	}
	p.published <- advertisement
	return &recordingPairingRegistration{}, nil
}

type recordingPairingRegistration struct{ shutdowns atomic.Int32 }

func (r *recordingPairingRegistration) Shutdown() { r.shutdowns.Add(1) }

type queuedListener struct {
	mu          sync.Mutex
	connections []net.Conn
}

func (l *queuedListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.connections) == 0 {
		return nil, net.ErrClosed
	}
	connection := l.connections[0]
	l.connections = l.connections[1:]
	return connection, nil
}

func (*queuedListener) Close() error   { return nil }
func (*queuedListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4zero} }

type blockingPairingListener struct {
	closeOnce sync.Once
	closed    chan struct{}
	address   *net.TCPAddr
}

func newBlockingPairingListener(port int) *blockingPairingListener {
	return &blockingPairingListener{
		closed:  make(chan struct{}),
		address: &net.TCPAddr{IP: net.IPv4zero, Port: port},
	}
}

func (l *blockingPairingListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *blockingPairingListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *blockingPairingListener) Addr() net.Addr { return l.address }

type addressedConn struct {
	net.Conn
	remote net.Addr
	closes atomic.Int32
}

func (c *addressedConn) Close() error {
	c.closes.Add(1)
	return nil
}

func (c *addressedConn) RemoteAddr() net.Addr { return c.remote }

type recordingManagedSSHAgent struct{ closes atomic.Int32 }

func (a *recordingManagedSSHAgent) Close() error {
	a.closes.Add(1)
	return nil
}

func TestLegacyPairRequiresExplicitUpgradeWithoutRawLANFallback(t *testing.T) {
	store := config.Store{Path: filepath.Join(t.TempDir(), "config.json")}
	legacy := config.Device{
		Name: "Windows", Address: "192.168.1.20", SSHPort: 49222, SyncPort: 49220,
		SSHHostPublicKey: "legacy-host-key", SyncthingDeviceID: "legacy-sync",
	}
	seed := config.Config{
		SchemaVersion: config.CurrentSchemaVersion, ActiveDevice: "legacy-windows",
		Devices: map[string]config.Device{"legacy-windows": legacy},
	}
	if err := store.Save(seed); err != nil {
		t.Fatalf("seed legacy config: %v", err)
	}
	problem := transportUpgradeProblem(store)
	if problem == nil || problem.Code != lifecycle.ProblemTransportUpgradeRequired ||
		problem.Message != lifecycle.TransportUpgradeMessage || problem.Action != lifecycle.TransportUpgradeAction {
		t.Fatalf("transport upgrade problem = %#v", problem)
	}
	client := newProductionTunnelClient(store, credentials.NewMemoryStore(), nil)
	if _, err := client.Dial(context.Background()); err == nil || !strings.Contains(err.Error(), "tunnel metadata") {
		t.Fatalf("legacy tunnel Dial() error = %v, want metadata rejection before network fallback", err)
	}
	after, err := store.Load()
	if err != nil || !reflect.DeepEqual(after, seed) {
		t.Fatalf("legacy trust changed without explicit forget: config=%#v err=%v", after, err)
	}
}
