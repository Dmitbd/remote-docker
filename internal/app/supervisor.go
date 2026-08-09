package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
)

const defaultSupervisorStopTimeout = 30 * time.Second

type SessionRuntime interface {
	Start(context.Context, lifecycle.Role) error
	Stop(context.Context, lifecycle.StopReason) error
	Done() <-chan error
}

type CrashWatchdog interface {
	CleanStop(context.Context) error
}

type WatchdogFactory func() (CrashWatchdog, error)

type SupervisorOption func(*Supervisor)

func WithSupervisorStopTimeout(timeout time.Duration) SupervisorOption {
	return func(supervisor *Supervisor) {
		if timeout > 0 {
			supervisor.stopTimeout = timeout
		}
	}
}

func WithWatchdogFactory(factory WatchdogFactory) SupervisorOption {
	return func(supervisor *Supervisor) { supervisor.watchdogFactory = factory }
}

// Supervisor is the only owner allowed to start and stop session
// infrastructure. Construction is inert and every launch begins paused.
type Supervisor struct {
	machine         *lifecycle.Machine
	runtime         SessionRuntime
	stopTimeout     time.Duration
	watchdogFactory WatchdogFactory

	mu       sync.Mutex
	running  bool
	stopping bool
	terminal bool
	watchdog CrashWatchdog
}

func NewSupervisor(machine *lifecycle.Machine, runtime SessionRuntime, options ...SupervisorOption) (*Supervisor, error) {
	if machine == nil || runtime == nil {
		return nil, errors.New("desktop supervisor dependencies are incomplete")
	}
	supervisor := &Supervisor{machine: machine, runtime: runtime, stopTimeout: defaultSupervisorStopTimeout}
	for _, option := range options {
		option(supervisor)
	}
	return supervisor, nil
}

func (s *Supervisor) Snapshot() lifecycle.Snapshot {
	if s == nil || s.machine == nil {
		return lifecycle.Snapshot{State: lifecycle.StateNeedsAction, ConnectionLimit: 1}
	}
	return s.machine.Snapshot()
}

func (s *Supervisor) Start(ctx context.Context) error {
	if s == nil {
		return errors.New("desktop supervisor is unavailable")
	}
	s.mu.Lock()
	if s.terminal {
		s.mu.Unlock()
		return errors.New("desktop supervisor is shutting down")
	}
	if s.running {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	var sessionWatchdog CrashWatchdog
	if s.watchdogFactory != nil {
		var err error
		sessionWatchdog, err = s.watchdogFactory()
		if err != nil {
			s.publishRuntimeProblem("watchdog_start_failed")
			return err
		}
	}
	role := s.machine.Snapshot().Role
	if err := s.runtime.Start(ctx, role); err != nil {
		if sessionWatchdog != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), s.stopTimeout)
			_ = sessionWatchdog.CleanStop(cleanupCtx)
			cancel()
		}
		s.publishRuntimeProblem("runtime_start_failed")
		return err
	}
	if _, err := s.machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled}); err != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), s.stopTimeout)
		_ = s.runtime.Stop(stopCtx, lifecycle.StopFailure)
		cancel()
		return err
	}
	s.mu.Lock()
	s.running = true
	s.watchdog = sessionWatchdog
	s.mu.Unlock()
	go s.observeRuntime(s.runtime.Done())
	return nil
}

func (s *Supervisor) Pause(ctx context.Context) error {
	if s == nil {
		return errors.New("desktop supervisor is unavailable")
	}
	s.mu.Lock()
	if !s.running && s.machine.Snapshot().State == lifecycle.StatePaused {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if _, err := s.machine.Apply(lifecycle.Event{Type: lifecycle.EventPauseRequested}); err != nil {
		return err
	}
	return s.stop(ctx, lifecycle.StopPause)
}

func (s *Supervisor) Disconnect(ctx context.Context, disconnect lifecycle.Disconnect) error {
	if s == nil {
		return errors.New("desktop supervisor is unavailable")
	}
	if _, err := s.machine.Apply(lifecycle.Event{Type: lifecycle.EventDisconnectRequested, Disconnect: &disconnect}); err != nil {
		return err
	}
	return s.stop(ctx, lifecycle.StopDisconnect)
}

func (s *Supervisor) Shutdown(ctx context.Context) error {
	if s == nil {
		return errors.New("desktop supervisor is unavailable")
	}
	s.mu.Lock()
	if s.terminal {
		s.mu.Unlock()
		return nil
	}
	s.terminal = true
	s.mu.Unlock()
	if _, err := s.machine.Apply(lifecycle.Event{Type: lifecycle.EventQuitRequested}); err != nil {
		return err
	}
	return s.stop(ctx, lifecycle.StopQuit)
}

func (s *Supervisor) stop(parent context.Context, reason lifecycle.StopReason) error {
	s.mu.Lock()
	s.stopping = true
	running := s.running
	sessionWatchdog := s.watchdog
	s.mu.Unlock()

	var stopErr error
	if running {
		if parent == nil {
			parent = context.Background()
		}
		stopCtx, cancel := context.WithTimeout(parent, s.stopTimeout)
		stopErr = s.runtime.Stop(stopCtx, reason)
		cancel()
	}
	var watchdogErr error
	if sessionWatchdog != nil {
		watchdogCtx, cancel := context.WithTimeout(context.Background(), s.stopTimeout)
		watchdogErr = sessionWatchdog.CleanStop(watchdogCtx)
		cancel()
	}

	s.mu.Lock()
	s.running = false
	s.stopping = false
	s.watchdog = nil
	s.mu.Unlock()
	stopErr = errors.Join(stopErr, watchdogErr)
	if stopErr != nil {
		s.publishRuntimeProblem("runtime_stop_failed")
		return stopErr
	}
	_, err := s.machine.Apply(lifecycle.Event{Type: lifecycle.EventStopCompleted})
	return err
}

func (s *Supervisor) observeRuntime(done <-chan error) {
	if done == nil {
		return
	}
	err, ok := <-done
	if !ok {
		err = nil
	}
	s.mu.Lock()
	unexpected := s.running && !s.stopping
	sessionWatchdog := s.watchdog
	if unexpected {
		s.stopping = true
	}
	s.mu.Unlock()
	if unexpected {
		_ = err
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), s.stopTimeout)
		_ = s.runtime.Stop(cleanupCtx, lifecycle.StopFailure)
		cancelCleanup()
		if sessionWatchdog != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), s.stopTimeout)
			_ = sessionWatchdog.CleanStop(cleanupCtx)
			cancel()
		}
		s.mu.Lock()
		s.running = false
		s.stopping = false
		s.watchdog = nil
		s.mu.Unlock()
		s.publishRuntimeProblem("runtime_stopped")
	}
}

func (s *Supervisor) publishRuntimeProblem(code string) {
	_, _ = s.machine.Apply(lifecycle.Event{Type: lifecycle.EventProblemDetected, Problem: &lifecycle.Problem{
		Code: code, Device: lifecycle.InitiatorLocal,
		Message: "Remote Docker runtime stopped unexpectedly.",
		Action:  "Open diagnostics and retry.",
	}})
}
