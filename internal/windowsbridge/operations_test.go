package windowsbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestManagedWSLOperationsUseOnlyExactAllowlistedInvocations(t *testing.T) {
	tests := []struct {
		operation managedWSLOperation
		binary    string
		args      []string
	}{
		{operation: operationListRunning, binary: "wsl.exe", args: []string{"--list", "--running", "--quiet"}},
		{operation: operationObserve, binary: "wsl.exe", args: []string{"--distribution", "remote-docker", "--exec", "/usr/local/bin/remote-docker-remote", "rpc"}},
		{operation: operationStartDistro, binary: "wsl.exe", args: []string{"--distribution", "remote-docker", "--exec", "/usr/local/bin/remote-docker-remote", "health"}},
		{operation: operationRestartSystemdTarget, binary: "wsl.exe", args: []string{"--distribution", "remote-docker", "--user", "root", "--exec", "/usr/bin/systemctl", "restart", "remote-docker.target"}},
		{operation: operationStopContainers, binary: "wsl.exe", args: []string{"--distribution", "remote-docker", "--exec", "/usr/local/bin/remote-docker-remote", "rpc"}},
		{operation: operationStopSystemdTarget, binary: "wsl.exe", args: []string{"--distribution", "remote-docker", "--user", "root", "--exec", "/usr/bin/systemctl", "stop", "remote-docker.target"}},
		{operation: operationTerminateDistro, binary: "wsl.exe", args: []string{"--terminate", "remote-docker"}},
	}
	for _, tt := range tests {
		t.Run(string(tt.operation), func(t *testing.T) {
			binary, args, ok := managedWSLInvocation(tt.operation)
			if !ok || binary != tt.binary || !reflect.DeepEqual(args, tt.args) {
				t.Fatalf("invocation = (%q, %#v, %t), want (%q, %#v, true)", binary, args, ok, tt.binary, tt.args)
			}
		})
	}
	if _, _, ok := managedWSLInvocation(managedWSLOperation("arbitrary-command")); ok {
		t.Fatal("arbitrary operation received a process invocation")
	}
}

func TestManagedWSLObservationDoesNotStartStoppedDistro(t *testing.T) {
	runner := &recordingManagedWSLRunner{runningOutput: "Ubuntu\x00\r\n"}
	status, err := (ManagedWSLOperations{runner: runner}).Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if status.Running || status.SystemdTarget || status.DiskAvailable {
		t.Fatalf("status = %#v, want stopped managed distro", status)
	}
	if !reflect.DeepEqual(runner.calls, []managedWSLOperation{operationListRunning}) {
		t.Fatalf("operations = %v, want read-only running query only", runner.calls)
	}
}

func TestManagedWSLObservationUsesTypedRemoteRPCForRunningDistro(t *testing.T) {
	runner := &recordingManagedWSLRunner{
		runningOutput: "remote-docker\x00\r\n",
		observation: ManagedWSLStatus{
			Running: true, SystemdTarget: true, DockerSocket: true,
			DiskAvailable: true, SyncthingService: true, PresenceActive: true,
		},
	}
	status, err := (ManagedWSLOperations{runner: runner}).Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if status != runner.observation {
		t.Fatalf("status = %#v, want %#v", status, runner.observation)
	}
	if !reflect.DeepEqual(runner.calls, []managedWSLOperation{operationListRunning, operationObserve}) {
		t.Fatalf("operations = %v, want ordered typed observation", runner.calls)
	}
	if runner.requestMethod != "diagnostics.observe" {
		t.Fatalf("RPC method = %q, want diagnostics.observe", runner.requestMethod)
	}
}

func TestManagedWSLRecoveryUsesTypedStartAndSystemdOperations(t *testing.T) {
	runner := &recordingManagedWSLRunner{}
	operations := ManagedWSLOperations{runner: runner}
	if err := operations.StartDistro(context.Background()); err != nil {
		t.Fatalf("StartDistro() error = %v", err)
	}
	if err := operations.RestartSystemdTarget(context.Background()); err != nil {
		t.Fatalf("RestartSystemdTarget() error = %v", err)
	}
	want := []managedWSLOperation{operationStartDistro, operationRestartSystemdTarget}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("operations = %v, want %v", runner.calls, want)
	}
}

func TestManagedWSLStopUsesGracefulOrderedCleanupForExactDistro(t *testing.T) {
	runner := &recordingManagedWSLRunner{}
	report, err := (ManagedWSLOperations{runner: runner}).StopManagedRuntime(context.Background())
	if err != nil {
		t.Fatalf("StopManagedRuntime() error = %v", err)
	}
	want := []managedWSLOperation{operationStopContainers, operationStopSystemdTarget, operationTerminateDistro}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("operations = %v, want %v", runner.calls, want)
	}
	if !report.ContainersStopped || !report.TargetStopped || !report.DistroTerminated {
		t.Fatalf("stop report = %#v", report)
	}
	if runner.requestMethod != "runtime.stop-containers" {
		t.Fatalf("stop RPC method = %q", runner.requestMethod)
	}
}

type recordingManagedWSLRunner struct {
	calls         []managedWSLOperation
	runningOutput string
	observation   ManagedWSLStatus
	requestMethod string
}

func (r *recordingManagedWSLRunner) Run(_ context.Context, operation managedWSLOperation, stdin io.Reader, stdout io.Writer) error {
	r.calls = append(r.calls, operation)
	switch operation {
	case operationListRunning:
		_, _ = io.WriteString(stdout, r.runningOutput)
	case operationObserve, operationStopContainers:
		var request struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(stdin).Decode(&request)
		r.requestMethod = request.Method
		result := any(r.observation)
		if operation == operationStopContainers {
			result = map[string]any{"stopped": true}
		}
		response := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  result,
		}
		var encoded bytes.Buffer
		_ = json.NewEncoder(&encoded).Encode(response)
		_, _ = io.Copy(stdout, strings.NewReader(encoded.String()))
	}
	return nil
}
