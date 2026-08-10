package desktop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/Dmitbd/remote-docker/internal/desktopui"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/processowner"
)

type UIProcess interface {
	Show(context.Context) error
	Stop(context.Context) error
	Running() bool
}

type ProcessLauncher struct {
	Executable string
	Owner      processowner.Owner

	mu        sync.Mutex
	process   *exec.Cmd
	done      chan struct{}
	exitError error
	arguments []string
	command   func(string, ...string) *exec.Cmd
	focus     func(context.Context) error
}

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
	p.mu.Lock()
	if p.process != nil && p.process.Process != nil && p.process.ProcessState == nil {
		focus := p.focus
		p.mu.Unlock()
		if focus == nil {
			focus = focusUIProcess
		}
		return focus(ctx)
	}
	commandFactory := p.command
	if commandFactory == nil {
		commandFactory = exec.Command
	}
	command := commandFactory(p.Executable, p.arguments...)
	if err := command.Start(); err != nil {
		p.mu.Unlock()
		return errors.New("start desktop UI process")
	}
	done := make(chan struct{})
	p.process = command
	p.done = done
	p.exitError = nil
	p.mu.Unlock()
	go func() {
		exitError := command.Wait()
		p.mu.Lock()
		if p.process == command {
			p.process = nil
			p.exitError = exitError
			close(done)
		}
		p.mu.Unlock()
	}()
	return nil
}

func (p *ProcessLauncher) Stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	command := p.process
	done := p.done
	p.mu.Unlock()
	if command == nil || command.Process == nil || done == nil {
		return nil
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		_ = command.Process.Kill()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		_ = command.Process.Kill()
		<-done
		return ctx.Err()
	}
}

func (p *ProcessLauncher) Running() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.process != nil && p.process.Process != nil && p.process.ProcessState == nil
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
