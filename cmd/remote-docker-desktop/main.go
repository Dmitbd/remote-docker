package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/Dmitbd/remote-docker/internal/app"
	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/desktop"
	"github.com/Dmitbd/remote-docker/internal/lifecycle"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/watchdog"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == watchdog.InternalArgument {
		os.Exit(watchdog.RunChild(context.Background(), os.Stdin))
	}
	maintenanceCtx, maintenanceCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	handled, maintenanceErr := runMaintenanceCommand(maintenanceCtx, os.Args[1:], productionMaintenanceDependencies())
	maintenanceCancel()
	if handled {
		if maintenanceErr != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 {
		os.Exit(2)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Remote Docker could not start.")
		os.Exit(1)
	}
}

func run() error {
	rootCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt)
	ctx, cancel := context.WithCancel(rootCtx)
	defer stopSignals()
	defer cancel()

	home, err := os.UserHomeDir()
	if err != nil {
		return errors.New("find application data directory")
	}
	configPath := config.DefaultPath(runtime.GOOS, home)
	instance, err := desktop.AcquireSingleInstance(filepath.Join(filepath.Dir(configPath), "desktop.lock"))
	if errors.Is(err, desktop.ErrAlreadyRunning) {
		var result map[string]any
		focusCtx, focusCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer focusCancel()
		_ = (localapi.Client{}).Call(focusCtx, localapi.MethodShowWindow, nil, &result)
		return nil
	}
	if err != nil {
		return err
	}
	defer instance.Close()

	owner, err := app.AttachProductionProcessOwner()
	if err != nil || !owner.Active() {
		return errors.New("attach process owner")
	}
	executable, err := os.Executable()
	if err != nil {
		return errors.New("locate desktop executable")
	}
	runtimeOwner, err := app.NewProductionAgentRuntime(app.ProductionAgentOptions{ConfigPath: configPath, ExecutablePath: executable})
	if err != nil {
		return err
	}
	role := lifecycle.RoleMacClient
	if runtime.GOOS == "windows" {
		role = lifecycle.RoleWindowsHost
	}
	deviceName, err := os.Hostname()
	if err != nil || deviceName == "" {
		deviceName = "Это устройство"
	}
	options := []lifecycle.Option{}
	if peer := initialTrustedPeer(config.Store{Path: configPath}, role); peer != nil {
		options = append(options, lifecycle.WithTrustedPeer(*peer))
	}
	machine, err := lifecycle.NewMachine(role, deviceName, options...)
	if err != nil {
		return err
	}
	if err := runtimeOwner.BindLifecycle(machine, "dev"); err != nil {
		return err
	}
	watchdogFactory, err := app.ProductionWatchdogFactory(executable)
	if err != nil {
		return err
	}
	supervisor, err := app.NewSupervisor(machine, runtimeOwner, app.WithWatchdogFactory(watchdogFactory))
	if err != nil {
		return err
	}
	controller, err := app.NewDesktopController(supervisor, runtimeOwner.Agent())
	if err != nil {
		return err
	}
	updates, unsubscribe := machine.Subscribe()
	defer unsubscribe()

	listener, err := localapi.Listen("")
	if err != nil {
		return err
	}
	defer listener.Close()
	handler := &desktopAPIHandler{base: controller}
	serveDone := make(chan error, 1)
	go func() { serveDone <- (localapi.Server{Handler: handler}).Serve(ctx, listener) }()

	uiProcess := &desktop.ProcessLauncher{Executable: uiExecutablePath(executable), Owner: owner}
	application, err := desktop.NewApplication(desktop.ApplicationOptions{
		UI: uiProcess, Snapshot: machine.Snapshot, Updates: updates, Platform: runtime.GOOS,
		OnPause: func(pauseCtx context.Context) error {
			_, pauseErr := handler.Handle(pauseCtx, localapi.MethodPause, nil)
			return pauseErr
		},
		OnQuit: func(quitCtx context.Context) error {
			_, quitErr := handler.Handle(quitCtx, localapi.MethodShutdown, nil)
			return quitErr
		},
	})
	if err != nil {
		return err
	}
	handler.setShow(application.Show)
	handler.setShutdown(func() {
		cancel()
		_ = listener.Close()
	})
	shutdownDone := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, shutdownErr := controller.Handle(shutdownCtx, localapi.MethodShutdown, nil)
		shutdownCancel()
		_ = application.Quit(shutdownCtx)
		shutdownDone <- shutdownErr
	}()
	if err := application.Run(ctx); err != nil {
		cancel()
		_ = listener.Close()
		return err
	}
	cancel()
	_ = listener.Close()
	select {
	case <-serveDone:
	case <-time.After(2 * time.Second):
	}
	if err := waitForDesktopShutdown(shutdownDone, 32*time.Second); err != nil {
		return fmt.Errorf("stop desktop runtime: %w", err)
	}
	return nil
}

func uiExecutablePath(desktopExecutable string) string {
	name := "remote-docker-ui"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(filepath.Dir(desktopExecutable), name)
}

func waitForDesktopShutdown(done <-chan error, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func initialTrustedPeer(store config.Store, role lifecycle.Role) *lifecycle.Peer {
	cfg, err := store.Load()
	if err != nil || cfg.ActiveDevice == "" {
		return nil
	}
	device, ok := cfg.Devices[cfg.ActiveDevice]
	if !ok {
		return nil
	}
	peerOS := "windows"
	if role == lifecycle.RoleWindowsHost {
		peerOS = "macos"
	}
	return &lifecycle.Peer{ID: cfg.ActiveDevice, Name: device.Name, OS: peerOS, Address: device.Address}
}

type desktopAPIHandler struct {
	base     localapi.Handler
	mu       sync.RWMutex
	show     func()
	shutdown func()
}

func (h *desktopAPIHandler) setShutdown(shutdown func()) {
	h.mu.Lock()
	h.shutdown = shutdown
	h.mu.Unlock()
}

func (h *desktopAPIHandler) setShow(show func()) {
	h.mu.Lock()
	h.show = show
	h.mu.Unlock()
}

func (h *desktopAPIHandler) Handle(ctx context.Context, method localapi.Method, params json.RawMessage) (any, error) {
	if method == localapi.MethodShowWindow {
		h.mu.RLock()
		show := h.show
		h.mu.RUnlock()
		if show != nil {
			show()
		}
		return map[string]bool{"shown": true}, nil
	}
	result, err := h.base.Handle(ctx, method, params)
	if err == nil && method == localapi.MethodShutdown {
		h.mu.RLock()
		shutdown := h.shutdown
		h.mu.RUnlock()
		if shutdown != nil {
			shutdown()
		}
	}
	return result, err
}

var _ localapi.Handler = (*desktopAPIHandler)(nil)
