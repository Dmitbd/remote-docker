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

	mu        sync.Mutex
	running   bool
	stopping  bool
	terminal  bool
	watchdog  CrashWatchdog
	startup   *supervisorStartup
	nextStart uint64
}

type supervisorStartup struct {
	generation     uint64
	cancel         context.CancelFunc
	done           chan struct{}
	cancelled      bool
	runtimeInvoked bool
	watchdog       CrashWatchdog
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
	if s.running || s.startup != nil {
		s.mu.Unlock()
		return nil
	}
	snapshot := s.machine.Snapshot()
	reservedStart := snapshot.State == lifecycle.StateConnecting && snapshot.ActionInProgress
	if ctx == nil {
		ctx = context.Background()
	}
	startCtx, cancelStart := context.WithCancel(ctx)
	s.nextStart++
	startup := &supervisorStartup{generation: s.nextStart, cancel: cancelStart, done: make(chan struct{})}
	s.startup = startup
	s.mu.Unlock()

	var sessionWatchdog CrashWatchdog
	if s.watchdogFactory != nil {
		var err error
		sessionWatchdog, err = s.watchdogFactory()
		if err != nil {
			if !reservedStart {
				s.publishRuntimeProblem("watchdog_start_failed")
			}
			cancelStart()
			s.finishStartup(startup)
			return err
		}
		s.mu.Lock()
		startup.watchdog = sessionWatchdog
		cancelled := startup.cancelled
		s.mu.Unlock()
		if cancelled {
			cancelStart()
			s.finishStartup(startup)
			return context.Canceled
		}
	}
	role := s.machine.Snapshot().Role
	s.mu.Lock()
	if startup.cancelled {
		s.mu.Unlock()
		cancelStart()
		s.finishStartup(startup)
		return context.Canceled
	}
	startup.runtimeInvoked = true
	s.mu.Unlock()
	startErr := s.runtime.Start(startCtx, role)
	s.mu.Lock()
	cancelled := startup.cancelled
	s.mu.Unlock()
	if startErr != nil {
		if cancelled {
			s.finishStartup(startup)
			return startErr
		}
		cancelStart()
		if sessionWatchdog != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), s.stopTimeout)
			_ = sessionWatchdog.CleanStop(cleanupCtx)
			cancel()
		}
		if !reservedStart {
			s.publishRuntimeStartProblem(startErr)
		}
		s.finishStartup(startup)
		return startErr
	}
	if cancelled {
		cancelStart()
		s.finishStartup(startup)
		return context.Canceled
	}
	if !reservedStart {
		if _, err := s.machine.Apply(lifecycle.Event{Type: lifecycle.EventEnabled}); err != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), s.stopTimeout)
			_ = s.runtime.Stop(stopCtx, lifecycle.StopFailure)
			cancel()
			cancelStart()
			s.finishStartup(startup)
			return err
		}
	}
	s.mu.Lock()
	if startup.cancelled {
		s.mu.Unlock()
		cancelStart()
		s.finishStartup(startup)
		return context.Canceled
	}
	s.running = true
	s.watchdog = sessionWatchdog
	s.mu.Unlock()
	s.finishStartup(startup)
	go s.observeRuntime(s.runtime.Done())
	return nil
}

func (s *Supervisor) finishStartup(startup *supervisorStartup) {
	s.mu.Lock()
	if s.startup == startup && s.startup.generation == startup.generation {
		s.startup = nil
	}
	close(startup.done)
	s.mu.Unlock()
}

type lifecycleProblemProvider interface {
	LifecycleProblem() lifecycle.Problem
}

func (s *Supervisor) publishRuntimeStartProblem(err error) {
	if s.publishTypedRuntimeStartProblem(err) {
		return
	}
	s.publishRuntimeProblem("runtime_start_failed")
}

func (s *Supervisor) publishTypedRuntimeStartProblem(err error) bool {
	var provider lifecycleProblemProvider
	if !errors.As(err, &provider) {
		return false
	}
	problem := provider.LifecycleProblem()
	_, applyErr := s.machine.Apply(lifecycle.Event{Type: lifecycle.EventProblemDetected, Problem: &problem})
	return applyErr == nil
}

// AbortConnectionStart keeps the lifecycle non-forgettable until every owned
// startup worker has stopped and joined. It is intentionally independent of
// the request context because a cancelled UI request must not release trust
// cleanup while recovery work can still recreate managed artifacts.
func (s *Supervisor) AbortConnectionStart() error {
	if s == nil {
		return errors.New("desktop supervisor is unavailable")
	}
	s.mu.Lock()
	s.stopping = true
	sessionWatchdog := s.watchdog
	s.mu.Unlock()
	if _, err := s.machine.Apply(lifecycle.Event{Type: lifecycle.EventConnectionStartAbortRequested}); err != nil {
		s.mu.Lock()
		s.stopping = false
		s.mu.Unlock()
		return err
	}
	stopErr := s.runtime.Stop(context.Background(), lifecycle.StopFailure)
	var watchdogErr error
	if sessionWatchdog != nil {
		watchdogErr = sessionWatchdog.CleanStop(context.Background())
	}
	s.mu.Lock()
	s.running = false
	s.stopping = false
	s.watchdog = nil
	s.mu.Unlock()
	if err := errors.Join(stopErr, watchdogErr); err != nil {
		return err
	}
	_, err := s.machine.Apply(lifecycle.Event{Type: lifecycle.EventStopCompleted})
	return err
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

func (s *Supervisor) CancelConnection(ctx context.Context) error {
	if s == nil {
		return errors.New("desktop supervisor is unavailable")
	}
	s.mu.Lock()
	if s.stopping || s.machine.Snapshot().State == lifecycle.StateStopping {
		s.mu.Unlock()
		return nil
	}
	if _, err := s.machine.Apply(lifecycle.Event{Type: lifecycle.EventConnectionCancelRequested}); err != nil {
		s.mu.Unlock()
		return err
	}
	s.stopping = true
	startup := s.startup
	if startup != nil {
		startup.cancelled = true
		startup.cancel()
	}
	s.mu.Unlock()
	if startup != nil {
		<-startup.done
		s.mu.Lock()
		forceRuntime := startup.runtimeInvoked
		if s.watchdog == nil {
			s.watchdog = startup.watchdog
		}
		s.mu.Unlock()
		return s.stopConnection(ctx, forceRuntime)
	}
	return s.stopConnection(ctx, false)
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
	return s.stopOwned(parent, reason, false, false)
}

func (s *Supervisor) stopConnection(parent context.Context, forceRuntime bool) error {
	return s.stopOwned(parent, lifecycle.StopCancelConnection, forceRuntime, true)
}

func (s *Supervisor) stopOwned(parent context.Context, reason lifecycle.StopReason, forceRuntime, retryConnection bool) error {
	s.mu.Lock()
	s.stopping = true
	running := s.running || forceRuntime
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

	combinedErr := errors.Join(stopErr, watchdogErr)
	s.mu.Lock()
	if stopErr == nil {
		s.running = false
	}
	if watchdogErr == nil {
		s.watchdog = nil
	}
	s.stopping = false
	s.mu.Unlock()
	if combinedErr != nil {
		if retryConnection {
			_, applyErr := s.machine.Apply(lifecycle.Event{Type: lifecycle.EventStopFailed})
			return errors.Join(combinedErr, applyErr)
		}
		s.publishRuntimeProblem("runtime_stop_failed")
		return combinedErr
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
