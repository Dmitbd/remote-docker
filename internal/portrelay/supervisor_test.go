package portrelay

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/sshtransport"
)

func TestSupervisorCreatesKeepsAndRemovesOneForwardPerPort(t *testing.T) {
	starter := &fakeForwardStarter{}
	supervisor := NewSupervisor(starter, time.Millisecond, 4*time.Millisecond)
	mapping := Mapping{Protocol: "tcp", LocalHost: "127.0.0.1", LocalPort: 8080, ContainerID: "one", RemoteHost: "0.0.0.0", RemotePort: 80}
	if err := supervisor.Apply(context.Background(), Snapshot{Mappings: []Mapping{mapping}}); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Apply(context.Background(), Snapshot{Mappings: []Mapping{mapping}}); err != nil {
		t.Fatal(err)
	}
	if starter.callCount() != 1 {
		t.Fatalf("starts = %d, want 1", starter.callCount())
	}
	if err := supervisor.Apply(context.Background(), Snapshot{}); err != nil {
		t.Fatal(err)
	}
	if !starter.process(0).closed {
		t.Fatal("removed forward remained running")
	}
}

func TestSupervisorRestartsExitedForwardWithBackoff(t *testing.T) {
	starter := &fakeForwardStarter{}
	supervisor := NewSupervisor(starter, time.Millisecond, 4*time.Millisecond)
	mapping := Mapping{Protocol: "tcp", LocalHost: "127.0.0.1", LocalPort: 8080, ContainerID: "one", RemotePort: 80}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := supervisor.Apply(ctx, Snapshot{Mappings: []Mapping{mapping}}); err != nil {
		t.Fatal(err)
	}
	starter.process(0).exit(errors.New("ssh disconnected"))
	deadline := time.Now().Add(time.Second)
	for starter.callCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if starter.callCount() != 2 {
		t.Fatalf("starts = %d, want retry", starter.callCount())
	}
	if err := supervisor.Apply(ctx, Snapshot{}); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorRejectsDuplicateLocalPortAndTypedPortConflict(t *testing.T) {
	starter := &fakeForwardStarter{}
	supervisor := NewSupervisor(starter, time.Millisecond, 4*time.Millisecond)
	err := supervisor.Apply(context.Background(), Snapshot{Mappings: []Mapping{
		{LocalHost: "127.0.0.1", LocalPort: 8080, ContainerID: "one", RemotePort: 80},
		{LocalHost: "127.0.0.1", LocalPort: 8080, ContainerID: "two", RemotePort: 80},
	}})
	var conflict *sshtransport.PortConflictError
	if !errors.As(err, &conflict) || conflict.Port != 8080 {
		t.Fatalf("Apply() error = %v", err)
	}
}

func TestSupervisorHealthIsReadOnlyAndRequiresEveryDesiredForwardActive(t *testing.T) {
	starter := &fakeForwardStarter{}
	supervisor := NewSupervisor(starter, time.Hour, time.Hour)
	if !supervisor.Healthy() {
		t.Fatal("empty initialized supervisor should be healthy")
	}
	mapping := Mapping{Protocol: "tcp", LocalHost: "127.0.0.1", LocalPort: 8080, ContainerID: "one", RemotePort: 80}
	if err := supervisor.Apply(context.Background(), Snapshot{Mappings: []Mapping{mapping}}); err != nil {
		t.Fatal(err)
	}
	if !supervisor.Healthy() {
		t.Fatal("active desired forward should be healthy")
	}
	starts := starter.callCount()
	starter.process(0).exit(errors.New("ssh disconnected"))
	deadline := time.Now().Add(time.Second)
	for supervisor.Healthy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if supervisor.Healthy() {
		t.Fatal("exited desired forward should be unhealthy before retry")
	}
	if starter.callCount() != starts {
		t.Fatalf("Healthy() started a process: starts=%d want=%d", starter.callCount(), starts)
	}
}

type fakeForwardStarter struct {
	mu        sync.Mutex
	processes []*fakeRelayProcess
}

func (s *fakeForwardStarter) Start(context.Context, Mapping) (ForwardProcess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	process := &fakeRelayProcess{done: make(chan struct{})}
	s.processes = append(s.processes, process)
	return process, nil
}

func (s *fakeForwardStarter) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processes)
}

func (s *fakeForwardStarter) process(index int) *fakeRelayProcess {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.processes[index]
}

type fakeRelayProcess struct {
	mu     sync.Mutex
	done   chan struct{}
	err    error
	closed bool
}

func (p *fakeRelayProcess) Done() <-chan struct{} { return p.done }
func (p *fakeRelayProcess) Err() error            { return p.err }
func (p *fakeRelayProcess) Close() error {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.done)
	}
	p.mu.Unlock()
	return nil
}
func (p *fakeRelayProcess) exit(err error) {
	p.err = err
	close(p.done)
}
