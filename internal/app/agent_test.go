package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/localapi"
)

func TestAgentStateMachineRequiresPinnedSSHDockerPingAndSyncthing(t *testing.T) {
	tests := []struct {
		name        string
		observation AgentObservation
		want        AgentState
	}{
		{name: "unpaired", observation: AgentObservation{}, want: AgentUnpaired},
		{name: "connecting", observation: AgentObservation{Paired: true}, want: AgentConnecting},
		{name: "engine starting", observation: AgentObservation{Paired: true, PinnedSSH: true}, want: AgentEngineStarting},
		{name: "syncing", observation: AgentObservation{Paired: true, PinnedSSH: true, DockerPing: true}, want: AgentSyncing},
		{name: "ready", observation: AgentObservation{Paired: true, PinnedSSH: true, DockerPing: true, SyncthingConnected: true}, want: AgentReady},
		{name: "needs action", observation: AgentObservation{Paired: true, NeedsAction: "pair again"}, want: AgentNeedsAction},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := NewAgent(&sequenceObserver{observations: []AgentObservation{tt.observation}}, nil, nil)
			status := agent.Refresh(context.Background())
			if status.State != tt.want {
				t.Fatalf("state = %q, want %q", status.State, tt.want)
			}
			if status.Paired != tt.observation.Paired {
				t.Fatalf("paired = %t, want %t", status.Paired, tt.observation.Paired)
			}
		})
	}
}

func TestAgentStateMachineDegradesAfterReadyProbeFailure(t *testing.T) {
	agent := NewAgent(&sequenceObserver{observations: []AgentObservation{
		{Paired: true, PinnedSSH: true, DockerPing: true, SyncthingConnected: true},
		{Paired: true, Err: errors.New("connection dropped")},
	}}, nil, nil)
	if got := agent.Refresh(context.Background()).State; got != AgentReady {
		t.Fatalf("initial state = %q", got)
	}
	status := agent.Refresh(context.Background())
	if status.State != AgentDegraded || !strings.Contains(status.Message, "connection") {
		t.Fatalf("degraded status = %#v", status)
	}
}

func TestAgentStatusHandlerPublishesExplicitPairedFlag(t *testing.T) {
	agent := NewAgent(&sequenceObserver{observations: []AgentObservation{{
		Paired: true, PinnedSSH: true, DockerPing: true, SyncthingConnected: true,
	}}}, nil, nil)
	agent.Refresh(context.Background())
	result, err := agent.Handle(context.Background(), localapi.MethodStatus, nil)
	if err != nil {
		t.Fatalf("Handle(Status) error = %v", err)
	}
	status, ok := result.(localapi.StatusResult)
	if !ok || !status.Paired || status.State != string(AgentReady) {
		t.Fatalf("status = %#v", result)
	}
}

func TestAgentReconnectRestoresInfrastructureWithoutDockerCommandReplay(t *testing.T) {
	events := []string{}
	agent := NewAgent(
		&sequenceObserver{observations: []AgentObservation{{
			Paired: true, PinnedSSH: true, DockerPing: true, SyncthingConnected: true,
		}}},
		&recordingRestorer{events: &events},
		nil,
	)
	if err := agent.Reconnect(context.Background()); err != nil {
		t.Fatalf("Reconnect() error = %v", err)
	}
	if want := []string{"event-stream", "relays"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("reconnect events = %#v, want %#v", events, want)
	}
	if got := agent.Status().State; got != AgentReady {
		t.Fatalf("state = %q, want Ready", got)
	}
}

func TestAgentHandlerRoutesAllControlOperations(t *testing.T) {
	controller := &recordingController{}
	agent := NewAgent(&sequenceObserver{}, nil, controller)
	methods := []localapi.Method{
		localapi.MethodListDevices,
		localapi.MethodPairCandidates,
		localapi.MethodPairStart,
		localapi.MethodPairConfirm,
		localapi.MethodUnpair,
		localapi.MethodWorkspaceAdd,
		localapi.MethodWorkspaceList,
		localapi.MethodWorkspaceRemove,
		localapi.MethodSyncStatus,
		localapi.MethodDoctor,
		localapi.MethodRecover,
	}
	for _, method := range methods {
		if _, err := agent.Handle(context.Background(), method, json.RawMessage(`{}`)); err != nil {
			t.Fatalf("Handle(%s) error = %v", method, err)
		}
	}
	if !reflect.DeepEqual(controller.methods, methods) {
		t.Fatalf("methods = %#v, want %#v", controller.methods, methods)
	}
}

func TestAgentCLIControlCommandsAndJSONOutput(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		method localapi.Method
	}{
		{name: "status", args: []string{"status"}, method: localapi.MethodStatus},
		{name: "pair candidates", args: []string{"pair", "candidates"}, method: localapi.MethodPairCandidates},
		{name: "pair start", args: []string{"pair", "start", "host"}, method: localapi.MethodPairStart},
		{name: "pair confirm", args: []string{"pair", "confirm", "session", "123456"}, method: localapi.MethodPairConfirm},
		{name: "unpair", args: []string{"unpair", "device"}, method: localapi.MethodUnpair},
		{name: "workspace add", args: []string{"workspace", "add", "/workspace"}, method: localapi.MethodWorkspaceAdd},
		{name: "workspace list", args: []string{"workspace", "list"}, method: localapi.MethodWorkspaceList},
		{name: "workspace remove", args: []string{"workspace", "remove", "workspace-id"}, method: localapi.MethodWorkspaceRemove},
		{name: "sync status", args: []string{"sync", "status"}, method: localapi.MethodSyncStatus},
		{name: "doctor", args: []string{"doctor"}, method: localapi.MethodDoctor},
		{name: "recover", args: []string{"recover"}, method: localapi.MethodRecover},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &recordingControlClient{result: map[string]any{"ok": true}}
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			args := append(append([]string{}, tt.args...), "--json")
			code := RunRuntime(context.Background(), Runtime{
				ProgramName: "remote-docker", ControlClient: client,
			}, args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("code = %d stderr = %q", code, stderr.String())
			}
			if client.method != tt.method {
				t.Fatalf("method = %q, want %q", client.method, tt.method)
			}
			if strings.TrimSpace(stdout.String()) != `{"ok":true}` {
				t.Fatalf("stdout = %q", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestAgentCLIUsesHumanStdoutAndStableErrorExitCodes(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	client := &recordingControlClient{result: localapi.StatusResult{State: "Ready", Message: "connected"}}
	code := RunRuntime(context.Background(), Runtime{ProgramName: "remote-docker", ControlClient: client}, []string{"status"}, &stdout, &stderr)
	if code != ExitOK || strings.TrimSpace(stdout.String()) != "Ready: connected" || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	client.err = &localapi.RemoteError{Code: localapi.ErrorNeedsAction, Message: "pair first"}
	code = RunRuntime(context.Background(), Runtime{ProgramName: "remote-docker", ControlClient: client}, []string{"doctor"}, &stdout, &stderr)
	if code != ExitNeedsAction || stdout.Len() != 0 || !strings.Contains(stderr.String(), "pair first") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

type sequenceObserver struct {
	observations []AgentObservation
	index        int
}

func (o *sequenceObserver) Observe(context.Context) AgentObservation {
	if len(o.observations) == 0 {
		return AgentObservation{}
	}
	if o.index >= len(o.observations) {
		return o.observations[len(o.observations)-1]
	}
	result := o.observations[o.index]
	o.index++
	return result
}

type recordingRestorer struct{ events *[]string }

func (r *recordingRestorer) RestoreEventStream(context.Context) error {
	*r.events = append(*r.events, "event-stream")
	return nil
}

func (r *recordingRestorer) RestoreRelays(context.Context) error {
	*r.events = append(*r.events, "relays")
	return nil
}

type recordingController struct{ methods []localapi.Method }

func (c *recordingController) Handle(_ context.Context, method localapi.Method, _ json.RawMessage) (any, error) {
	c.methods = append(c.methods, method)
	return map[string]bool{"ok": true}, nil
}

type recordingControlClient struct {
	method localapi.Method
	result any
	err    error
}

func (c *recordingControlClient) Call(_ context.Context, method localapi.Method, _ any, result any) error {
	c.method = method
	if c.err != nil {
		return c.err
	}
	raw, _ := json.Marshal(c.result)
	return json.Unmarshal(raw, result)
}
