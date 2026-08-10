package portrelay

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/dockercli"
)

func TestDockerSourceUsesOnlyInfrastructureQueries(t *testing.T) {
	executor := &sourceExecutor{outputs: []string{
		"first\nsecond\n",
		`[
  {"Id":"first","State":{"Running":true},"NetworkSettings":{"Ports":{"80/tcp":[{"HostIp":"0.0.0.0","HostPort":"8080"}]}}},
  {"Id":"second","State":{"Running":false},"NetworkSettings":{"Ports":{}}}
]`,
	}}
	starter := &sourceEventStarter{output: `{"Type":"container","Action":"start","id":"first"}` + "\n"}
	source := DockerSource{
		CLI: "docker-real", Context: "remote-docker", Env: []string{"SAFE=1"},
		Executor: executor, EventStarter: starter,
	}

	containers, err := source.RunningContainers(context.Background())
	if err != nil {
		t.Fatalf("RunningContainers() error = %v", err)
	}
	if len(containers) != 2 || containers[0].ID != "first" {
		t.Fatalf("containers = %#v", containers)
	}
	wantCalls := [][]string{
		{"--context", "remote-docker", "container", "ls", "--quiet", "--filter", "status=running"},
		{"--context", "remote-docker", "inspect", "first", "second"},
	}
	if !reflect.DeepEqual(executor.calls, wantCalls) {
		t.Fatalf("Docker snapshot calls = %#v, want %#v", executor.calls, wantCalls)
	}

	events, err := source.Events(context.Background())
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	event, ok := <-events
	if !ok || event.Action != "start" || event.ContainerID != "first" {
		t.Fatalf("event = %#v, ok=%t", event, ok)
	}
	if _, ok := <-events; ok {
		t.Fatal("event stream remained open after process exit")
	}
	wantEventArgs := []string{"--context", "remote-docker", "events", "--format", "{{json .}}", "--filter", "type=container"}
	if !reflect.DeepEqual(starter.invocation.Args, wantEventArgs) || starter.invocation.Binary != "docker-real" {
		t.Fatalf("event invocation = %#v, want args %#v", starter.invocation, wantEventArgs)
	}
}

type sourceExecutor struct {
	outputs []string
	calls   [][]string
}

func (e *sourceExecutor) Run(_ context.Context, invocation dockercli.Invocation) error {
	e.calls = append(e.calls, append([]string(nil), invocation.Args...))
	if len(e.outputs) > 0 {
		_, _ = io.Copy(invocation.Stdout, strings.NewReader(e.outputs[0]))
		e.outputs = e.outputs[1:]
	}
	return nil
}

type sourceEventStarter struct {
	output     string
	invocation dockercli.Invocation
}

func (s *sourceEventStarter) Start(_ context.Context, invocation dockercli.Invocation) (EventProcess, error) {
	s.invocation = invocation
	return &sourceEventProcess{reader: io.NopCloser(bytes.NewBufferString(s.output))}, nil
}

type sourceEventProcess struct{ reader io.ReadCloser }

func (p *sourceEventProcess) Stdout() io.ReadCloser { return p.reader }
func (p *sourceEventProcess) Wait() error           { return nil }
