package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/Dmitbd/remote-docker/internal/desktopui"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/processowner"
)

type UIProcess interface {
	Show(context.Context) error
	Stop(context.Context) error
	Running() bool
}

var (
	errUIChildOwnershipChanged = errors.New("desktop UI child ownership changed")
	errUIProcessLauncherClosed = errors.New("desktop UI process launcher is closed")
)

type ProcessLauncher struct {
	Executable string
	Owner      processowner.Owner

	mu         sync.Mutex
	process    *exec.Cmd
	done       chan struct{}
	exitError  error
	generation uint64
	stopping   bool
	closed     bool
	show       *uiShowOperation
	arguments  []string
	command    func(string, ...string) *exec.Cmd
	focus      func(context.Context) error
	signal     func(*os.Process, os.Signal) error
	kill       func(*os.Process) error
	wait       func(*exec.Cmd) error
}

type uiShowOperation struct {
	done chan struct{}
	err  error
}

type uiChildSnapshot struct {
	command    *exec.Cmd
	process    *os.Process
	done       chan struct{}
	generation uint64
}

const uiFocusAttemptLimit = 250 * time.Millisecond
const uiExitObservationLimit = 250 * time.Millisecond

func (p *ProcessLauncher) Show(ctx context.Context) error {
	if p == nil || p.Owner == nil || !p.Owner.Active() {
		return errors.New("desktop UI process owner is unavailable")
	}
	if !filepath.IsAbs(p.Executable) {
		return errors.New("desktop UI executable path must be absolute")
	}
	info, err := os.Stat(p.Executable)
	if err != nil || info.IsDir() {
		return errors.New("desktop UI executable is unavailable")
	}
	operation, leader, err := p.beginShow()
	if err != nil {
		return err
	}
	if !leader {
		select {
		case <-operation.done:
			return operation.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	err = p.showOnce(ctx)
	p.finishShow(operation, err)
	return err
}

func (p *ProcessLauncher) beginShow() (*uiShowOperation, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, false, errUIProcessLauncherClosed
	}
	if p.show != nil {
		return p.show, false, nil
	}
	operation := &uiShowOperation{done: make(chan struct{})}
	p.show = operation
	return operation, true, nil
}

func (p *ProcessLauncher) finishShow(operation *uiShowOperation, err error) {
	p.mu.Lock()
	operation.err = err
	if p.show == operation {
		p.show = nil
	}
	close(operation.done)
	p.mu.Unlock()
}

func (p *ProcessLauncher) showOnce(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errUIProcessLauncherClosed
	}
	if p.process != nil && p.process.Process != nil {
		if p.stopping {
			p.mu.Unlock()
			return errUIChildOwnershipChanged
		}
		child := uiChildSnapshot{
			command:    p.process,
			process:    p.process.Process,
			done:       p.done,
			generation: p.generation,
		}
		focus := p.focus
		p.mu.Unlock()
		if focus == nil {
			focus = focusUIProcess
		}
		focusCtx, cancelFocus := boundedFocusContext(ctx)
		focusErr := focus(focusCtx)
		cancelFocus()
		if focusErr == nil {
			p.mu.Lock()
			owned := p.ownsChildLocked(child)
			p.mu.Unlock()
			if !owned {
				return errUIChildOwnershipChanged
			}
			return nil
		} else if recoveryErr := p.recoverChild(ctx, child); recoveryErr != nil {
			return fmt.Errorf("recover desktop UI after focus failure: %w", recoveryErr)
		}
		return nil
	}
	err := p.startLocked()
	p.mu.Unlock()
	return err
}

func boundedFocusContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.WithCancel(ctx)
	}
	focusLimit := remaining / 2
	if focusLimit > uiFocusAttemptLimit {
		focusLimit = uiFocusAttemptLimit
	}
	return context.WithTimeout(ctx, focusLimit)
}

func (p *ProcessLauncher) startLocked() error {
	if p.closed {
		return errUIProcessLauncherClosed
	}
	commandFactory := p.command
	if commandFactory == nil {
		commandFactory = exec.Command
	}
	command := commandFactory(p.Executable, p.arguments...)
	if err := command.Start(); err != nil {
		return errors.New("start desktop UI process")
	}
	done := make(chan struct{})
	p.generation++
	p.process = command
	p.done = done
	p.exitError = nil
	p.stopping = false
	waitProcess := p.wait
	if waitProcess == nil {
		waitProcess = func(command *exec.Cmd) error { return command.Wait() }
	}
	go func() {
		exitError := waitProcess(command)
		p.mu.Lock()
		if p.process == command && p.done == done {
			p.process = nil
			p.exitError = exitError
			p.stopping = false
			close(done)
		}
		p.mu.Unlock()
	}()
	return nil
}

func (p *ProcessLauncher) recoverChild(ctx context.Context, child uiChildSnapshot) error {
	if child.command == nil || child.process == nil || child.done == nil {
		return errors.New("desktop UI child ownership is unavailable")
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errUIProcessLauncherClosed
	}
	if !p.ownsChildLocked(child) || p.stopping {
		if p.childExitedWithoutReplacementLocked(child) {
			err := p.startLocked()
			p.mu.Unlock()
			return err
		}
		p.mu.Unlock()
		return errUIChildOwnershipChanged
	}
	p.stopping = true
	signalProcess := p.signal
	if signalProcess == nil {
		signalProcess = func(process *os.Process, signal os.Signal) error { return process.Signal(signal) }
	}
	killProcess := p.kill
	if killProcess == nil {
		killProcess = func(process *os.Process) error { return process.Kill() }
	}
	p.mu.Unlock()
	signalErr := signalProcess(child.process, os.Interrupt)
	if signalErr != nil {
		killErr := killProcess(child.process)
		if killErr != nil {
			if p.observeExactChildExit(ctx, child.done) == nil {
				return p.finishRecovery(ctx, child)
			}
			p.releaseStopping(child)
			return errors.Join(fmt.Errorf("signal desktop UI process: %w", signalErr), fmt.Errorf("kill desktop UI process: %w", killErr))
		}
		return p.finishRecovery(ctx, child)
	}
	return p.finishRecovery(ctx, child)
}

func (p *ProcessLauncher) finishRecovery(ctx context.Context, child uiChildSnapshot) error {
	select {
	case <-child.done:
	case <-ctx.Done():
		p.mu.Lock()
		if !p.ownsChildLocked(child) {
			p.mu.Unlock()
			return errors.Join(ctx.Err(), errUIChildOwnershipChanged)
		}
		killProcess := p.kill
		if killProcess == nil {
			killProcess = func(process *os.Process) error { return process.Kill() }
		}
		p.mu.Unlock()
		killErr := killProcess(child.process)
		if killErr != nil {
			p.releaseStopping(child)
			return errors.Join(ctx.Err(), fmt.Errorf("kill desktop UI process: %w", killErr))
		}
		return ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.generation != child.generation || p.process != nil || p.done != child.done {
		return errUIChildOwnershipChanged
	}
	return p.startLocked()
}

func (p *ProcessLauncher) observeExactChildExit(ctx context.Context, done <-chan struct{}) error {
	observeCtx, cancel := context.WithTimeout(ctx, uiExitObservationLimit)
	defer cancel()
	select {
	case <-done:
		return nil
	case <-observeCtx.Done():
		return observeCtx.Err()
	}
}

func (p *ProcessLauncher) releaseStopping(child uiChildSnapshot) {
	p.mu.Lock()
	if p.sameChildLocked(child) {
		p.stopping = false
	}
	p.mu.Unlock()
}

func (p *ProcessLauncher) ownsChildLocked(child uiChildSnapshot) bool {
	return p.sameChildLocked(child) && p.generation == child.generation
}

func (p *ProcessLauncher) sameChildLocked(child uiChildSnapshot) bool {
	return p.process == child.command && p.process != nil && p.process.Process == child.process && p.done == child.done
}

func (p *ProcessLauncher) childExitedWithoutReplacementLocked(child uiChildSnapshot) bool {
	if p.process != nil || p.done != child.done || p.generation != child.generation {
		return false
	}
	select {
	case <-child.done:
		return true
	default:
		return false
	}
}

func (p *ProcessLauncher) Stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	p.closed = true
	child := uiChildSnapshot{command: p.process, done: p.done, generation: p.generation}
	if p.process != nil {
		child.process = p.process.Process
		if p.stopping {
			done := p.done
			p.mu.Unlock()
			select {
			case <-done:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		p.generation++
		p.stopping = true
		child.generation = p.generation
	}
	p.mu.Unlock()
	if child.command == nil || child.process == nil || child.done == nil {
		return nil
	}
	signalProcess := p.signal
	if signalProcess == nil {
		signalProcess = func(process *os.Process, signal os.Signal) error { return process.Signal(signal) }
	}
	killProcess := p.kill
	if killProcess == nil {
		killProcess = func(process *os.Process) error { return process.Kill() }
	}
	if err := signalProcess(child.process, os.Interrupt); err != nil {
		if killErr := killProcess(child.process); killErr != nil {
			if p.observeExactChildExit(ctx, child.done) == nil {
				return nil
			}
			p.releaseStopping(child)
			return errors.Join(fmt.Errorf("signal desktop UI process: %w", err), fmt.Errorf("kill desktop UI process: %w", killErr))
		}
		select {
		case <-child.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case <-child.done:
		return nil
	case <-ctx.Done():
		if err := killProcess(child.process); err != nil {
			p.releaseStopping(child)
			return errors.Join(ctx.Err(), fmt.Errorf("kill desktop UI process: %w", err))
		}
		return ctx.Err()
	}
}

func (p *ProcessLauncher) Running() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.process != nil && p.process.Process != nil
}

func focusUIProcess(ctx context.Context) error {
	endpoint, err := desktopui.FocusEndpoint()
	if err != nil {
		return errors.New("locate desktop UI focus endpoint")
	}
	var result map[string]bool
	if err := (localapi.Client{Endpoint: endpoint}).Call(ctx, localapi.MethodShowWindow, nil, &result); err != nil {
		return errors.New("focus desktop UI process")
	}
	return nil
}

var _ UIProcess = (*ProcessLauncher)(nil)
