package portrelay

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Dmitbd/remote-docker/internal/sshtransport"
)

// ForwardProcess is one app-owned SSH forwarding child.
type ForwardProcess interface {
	Done() <-chan struct{}
	Err() error
	Close() error
}

// ForwardStarter creates one loopback-only forward.
type ForwardStarter interface {
	Start(context.Context, Mapping) (ForwardProcess, error)
}

// SSHForwardStarter adapts discovered Docker publications to strict local forwards.
type SSHForwardStarter struct {
	Forwarder   sshtransport.Forwarder
	ConfigPath  string
	ManagedHost string
}

func (s SSHForwardStarter) Start(ctx context.Context, mapping Mapping) (ForwardProcess, error) {
	return s.Forwarder.Start(ctx, s.ConfigPath, s.ManagedHost, sshtransport.ForwardSpec{
		Direction:  sshtransport.ForwardLocal,
		LocalPort:  mapping.LocalPort,
		RemotePort: mapping.LocalPort,
	})
}

type activeForward struct {
	mapping Mapping
	process ForwardProcess
}

// Supervisor maintains at most one SSH process for each Mac loopback port.
type Supervisor struct {
	starter ForwardStarter
	minimum time.Duration
	maximum time.Duration

	mu       sync.Mutex
	desired  map[int]Mapping
	active   map[int]activeForward
	starting map[int]bool
}

// NewSupervisor creates an empty forwarding supervisor.
func NewSupervisor(starter ForwardStarter, minimum, maximum time.Duration) *Supervisor {
	if minimum <= 0 {
		minimum = 250 * time.Millisecond
	}
	if maximum < minimum {
		maximum = 5 * time.Second
	}
	return &Supervisor{
		starter: starter, minimum: minimum, maximum: maximum,
		desired: make(map[int]Mapping), active: make(map[int]activeForward), starting: make(map[int]bool),
	}
}

// Apply reconciles the complete desired snapshot and never kills foreign processes.
func (s *Supervisor) Apply(ctx context.Context, snapshot Snapshot) error {
	if s == nil || s.starter == nil {
		return errors.New("port forward supervisor is unavailable")
	}
	next := make(map[int]Mapping, len(snapshot.Mappings))
	for _, mapping := range snapshot.Mappings {
		if mapping.LocalHost != "127.0.0.1" || (mapping.Protocol != "" && mapping.Protocol != "tcp") || mapping.LocalPort < 1 {
			return fmt.Errorf("invalid loopback TCP mapping: %#v", mapping)
		}
		if existing, duplicate := next[mapping.LocalPort]; duplicate && existing.Key() != mapping.Key() {
			return &sshtransport.PortConflictError{Port: mapping.LocalPort}
		}
		next[mapping.LocalPort] = mapping
	}

	var stop []ForwardProcess
	s.mu.Lock()
	s.desired = next
	for port, entry := range s.active {
		wanted, exists := next[port]
		if !exists || wanted.Key() != entry.mapping.Key() {
			delete(s.active, port)
			stop = append(stop, entry.process)
		}
	}
	ports := make([]int, 0, len(next))
	for port := range next {
		if _, exists := s.active[port]; !exists && !s.starting[port] {
			ports = append(ports, port)
		}
	}
	s.mu.Unlock()
	for _, process := range stop {
		if err := process.Close(); err != nil {
			return fmt.Errorf("stop removed SSH forward: %w", err)
		}
	}
	sort.Ints(ports)
	for _, port := range ports {
		if err := s.start(ctx, next[port], 0); err != nil {
			return err
		}
	}
	return nil
}

func (s *Supervisor) start(ctx context.Context, mapping Mapping, attempt int) error {
	s.mu.Lock()
	wanted, exists := s.desired[mapping.LocalPort]
	if !exists || wanted.Key() != mapping.Key() || s.starting[mapping.LocalPort] {
		s.mu.Unlock()
		return nil
	}
	if _, active := s.active[mapping.LocalPort]; active {
		s.mu.Unlock()
		return nil
	}
	s.starting[mapping.LocalPort] = true
	s.mu.Unlock()

	process, err := s.starter.Start(ctx, mapping)
	s.mu.Lock()
	delete(s.starting, mapping.LocalPort)
	wanted, stillDesired := s.desired[mapping.LocalPort]
	if err == nil && stillDesired && wanted.Key() == mapping.Key() {
		s.active[mapping.LocalPort] = activeForward{mapping: mapping, process: process}
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if !stillDesired || wanted.Key() != mapping.Key() {
		return process.Close()
	}
	go s.monitor(ctx, mapping, process, attempt)
	return nil
}

func (s *Supervisor) monitor(ctx context.Context, mapping Mapping, process ForwardProcess, attempt int) {
	<-process.Done()
	s.mu.Lock()
	entry, active := s.active[mapping.LocalPort]
	if active && entry.process == process {
		delete(s.active, mapping.LocalPort)
	}
	wanted, stillDesired := s.desired[mapping.LocalPort]
	s.mu.Unlock()
	if !stillDesired || wanted.Key() != mapping.Key() || ctx.Err() != nil {
		return
	}

	delay := s.minimum
	for index := 0; index < attempt && delay < s.maximum; index++ {
		delay *= 2
		if delay > s.maximum {
			delay = s.maximum
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	if err := s.start(ctx, mapping, attempt+1); err != nil {
		go s.retry(ctx, mapping, attempt+1)
	}
}

func (s *Supervisor) retry(ctx context.Context, mapping Mapping, attempt int) {
	delay := s.minimum
	for index := 0; index < attempt && delay < s.maximum; index++ {
		delay *= 2
		if delay > s.maximum {
			delay = s.maximum
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	s.mu.Lock()
	wanted, exists := s.desired[mapping.LocalPort]
	s.mu.Unlock()
	if !exists || wanted.Key() != mapping.Key() {
		return
	}
	if err := s.start(ctx, mapping, attempt+1); err != nil {
		go s.retry(ctx, mapping, attempt+1)
	}
}
