package portrelay

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDecodeInspectDiscoversFixedLoopbackDynamicAndRejectsUDP(t *testing.T) {
	input := `[
  {
    "Id": "fixed",
    "State": {"Running": true},
    "NetworkSettings": {"Ports": {
      "80/tcp": [
        {"HostIp": "0.0.0.0", "HostPort": "8080"},
        {"HostIp": "0.0.0.0", "HostPort": "8080"}
      ]
    }}
  },
  {
    "Id": "dynamic",
    "State": {"Running": true},
    "NetworkSettings": {"Ports": {
      "80/tcp": [{"HostIp": "127.0.0.1", "HostPort": "49153"}]
    }}
  },
  {
    "Id": "stopped",
    "State": {"Running": false},
    "NetworkSettings": {"Ports": {
      "90/tcp": [{"HostIp": "0.0.0.0", "HostPort": "9090"}]
    }}
  },
  {
    "Id": "udp",
    "State": {"Running": true},
    "NetworkSettings": {"Ports": {
      "53/udp": [{"HostIp": "0.0.0.0", "HostPort": "5353"}]
    }}
  }
]`
	containers, err := DecodeInspect(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Discover(containers)
	want := []Mapping{
		{Protocol: "tcp", LocalHost: "127.0.0.1", LocalPort: 49153, ContainerID: "dynamic", RemoteHost: "127.0.0.1", RemotePort: 80},
		{Protocol: "tcp", LocalHost: "127.0.0.1", LocalPort: 8080, ContainerID: "fixed", RemoteHost: "0.0.0.0", RemotePort: 80},
	}
	if !reflect.DeepEqual(snapshot.Mappings, want) {
		t.Fatalf("Mappings = %#v, want %#v", snapshot.Mappings, want)
	}
	if len(snapshot.Unsupported) != 1 || snapshot.Unsupported[0].Protocol != "udp" || snapshot.Unsupported[0].ContainerID != "udp" {
		t.Fatalf("Unsupported = %#v", snapshot.Unsupported)
	}
	if snapshot.Mappings[0].Key() == snapshot.Mappings[1].Key() {
		t.Fatal("distinct mappings have the same key")
	}
}

func TestDecodeDockerEvents(t *testing.T) {
	for _, action := range []string{"create", "start", "die", "destroy"} {
		event, err := DecodeEvent([]byte(`{"Type":"container","Action":"` + action + `","id":"container-1"}`))
		if err != nil {
			t.Fatal(err)
		}
		if event.Action != action || event.ContainerID != "container-1" || !event.RequiresReconcile() {
			t.Fatalf("event = %#v", event)
		}
	}
	event, err := DecodeEvent([]byte(`{"Type":"container","Action":"exec_start","id":"container-1"}`))
	if err != nil || event.RequiresReconcile() {
		t.Fatalf("exec event = %#v error=%v", event, err)
	}
}

func TestReconcilerUsesFullRunningSnapshotForEveryLifecycleEvent(t *testing.T) {
	source := &fakeSource{containers: []Container{{ID: "first", Running: true}}}
	sink := &recordingSink{}
	reconciler := Reconciler{Source: source, Sink: sink}
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"create", "start", "die", "destroy"} {
		source.containers = []Container{{ID: action, Running: true}}
		if err := reconciler.HandleEvent(context.Background(), Event{Action: action}); err != nil {
			t.Fatal(err)
		}
	}
	if source.runningCalls != 5 || sink.calls != 5 {
		t.Fatalf("running calls=%d sink calls=%d", source.runningCalls, sink.calls)
	}
	if err := reconciler.HandleEvent(context.Background(), Event{Action: "exec_start"}); err != nil {
		t.Fatal(err)
	}
	if source.runningCalls != 5 {
		t.Fatal("irrelevant event triggered reconcile")
	}
}

func TestReconcilerRestartsEventStreamWithBoundedBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &fakeSource{streamErr: errors.New("events disconnected")}
	sink := &recordingSink{}
	var delays []time.Duration
	reconciler := Reconciler{
		Source:     source,
		Sink:       sink,
		MinBackoff: 10 * time.Millisecond,
		MaxBackoff: 40 * time.Millisecond,
		Sleep: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			if len(delays) == 4 {
				cancel()
				return context.Canceled
			}
			return nil
		},
	}
	if err := reconciler.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond}
	if !reflect.DeepEqual(delays, want) {
		t.Fatalf("delays = %#v, want %#v", delays, want)
	}
	if source.runningCalls != 4 || source.streamCalls != 4 {
		t.Fatalf("running calls=%d stream calls=%d", source.runningCalls, source.streamCalls)
	}
}

type fakeSource struct {
	containers   []Container
	streamErr    error
	runningCalls int
	streamCalls  int
}

func (s *fakeSource) RunningContainers(context.Context) ([]Container, error) {
	s.runningCalls++
	return append([]Container(nil), s.containers...), nil
}

func (s *fakeSource) Events(context.Context) (<-chan Event, error) {
	s.streamCalls++
	if s.streamErr != nil {
		return nil, s.streamErr
	}
	stream := make(chan Event)
	close(stream)
	return stream, nil
}

type recordingSink struct {
	calls int
	last  Snapshot
}

func (s *recordingSink) Apply(_ context.Context, snapshot Snapshot) error {
	s.calls++
	s.last = snapshot
	return nil
}
