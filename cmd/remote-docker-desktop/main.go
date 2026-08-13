package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
	if err := configureDesktopShell(runtime.GOOS, setAccessoryActivationPolicy); err != nil {
		return err
	}
	rootCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt)
	ctx, cancel := context.WithCancel(rootCtx)
	defer stopSignals()
	defer cancel()

	home, err := os.UserHomeDir()
	if err != nil {
		return errors.New("find application data directory")
	}
	configPath := config.DefaultPath(runtime.GOOS, home)
	instance, err := acquireDesktopUpgradeGate(ctx, runtime.GOOS, configPath, productionDesktopUpgradeDependencies())
	if errors.Is(err, desktop.ErrAlreadyRunning) {
		return showExistingDesktop(ctx, 2*time.Second, (localapi.Client{}).Call)
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
	})
	shutdownDone := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdownDone <- completeDesktopShutdown(
			func(shutdownCtx context.Context) error {
				_, shutdownErr := controller.Handle(shutdownCtx, localapi.MethodShutdown, nil)
				return shutdownErr
			},
			listener.Close,
			application.Quit,
		)
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

type desktopUpgradeDependencies struct {
	acquireInstance      func(string) (desktop.InstanceLock, error)
	stopLegacy           func(context.Context) error
	confirmLegacyStopped func(context.Context) error
	upgradeConfig        func(context.Context, string) error
}

func productionDesktopUpgradeDependencies() desktopUpgradeDependencies {
	return desktopUpgradeDependencies{
		acquireInstance: desktop.AcquireSingleInstance,
		stopLegacy: func(ctx context.Context) error {
			return stopLegacyWindowsDesktop(ctx, 50*time.Millisecond, func(callCtx context.Context, method localapi.Method) error {
				var result map[string]any
				return (localapi.Client{}).Call(callCtx, method, nil, &result)
			})
		},
		confirmLegacyStopped: func(ctx context.Context) error {
			executablePath, err := os.Executable()
			if err != nil {
				return errors.New("locate desktop executable for upgrade gate")
			}
			return desktop.WaitForNoOtherInstance(ctx, executablePath, 50*time.Millisecond)
		},
		upgradeConfig: app.UpgradeConfig,
	}
}

func acquireDesktopUpgradeGate(
	ctx context.Context,
	platform, configPath string,
	dependencies desktopUpgradeDependencies,
) (desktop.InstanceLock, error) {
	if dependencies.acquireInstance == nil || dependencies.upgradeConfig == nil {
		return nil, errors.New("desktop upgrade gate is incomplete")
	}
	instance, err := dependencies.acquireInstance(filepath.Join(filepath.Dir(configPath), "desktop.lock"))
	if err != nil {
		return nil, err
	}
	if platform == "windows" {
		if dependencies.stopLegacy == nil || dependencies.confirmLegacyStopped == nil {
			_ = instance.Close()
			return nil, errors.New("legacy desktop shutdown proof is unavailable")
		}
		if err := dependencies.stopLegacy(ctx); err != nil {
			_ = instance.Close()
			return nil, err
		}
		if err := dependencies.confirmLegacyStopped(ctx); err != nil {
			_ = instance.Close()
			return nil, err
		}
	}
	if err := dependencies.upgradeConfig(ctx, configPath); err != nil {
		_ = instance.Close()
		return nil, err
	}
	return instance, nil
}

func stopLegacyWindowsDesktop(
	ctx context.Context,
	interval time.Duration,
	call func(context.Context, localapi.Method) error,
) error {
	if call == nil {
		return errors.New("legacy desktop control is unavailable")
	}
	if interval <= 0 {
		interval = 50 * time.Millisecond
	}
	probeCtx, cancelProbe := context.WithTimeout(ctx, 500*time.Millisecond)
	err := call(probeCtx, localapi.MethodStatus)
	cancelProbe()
	if err != nil {
		if localapi.IsEndpointAbsent(err) {
			return nil
		}
		return fmt.Errorf("verify legacy Remote Docker process: %w", err)
	}
	for {
		shutdownCtx, cancelShutdown := context.WithTimeout(ctx, 2*time.Second)
		_ = call(shutdownCtx, localapi.MethodShutdown)
		cancelShutdown()
		probeCtx, cancelProbe = context.WithTimeout(ctx, 500*time.Millisecond)
		err = call(probeCtx, localapi.MethodStatus)
		cancelProbe()
		if err != nil {
			if localapi.IsEndpointAbsent(err) {
				return nil
			}
			return fmt.Errorf("wait for legacy Remote Docker process: %w", err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return errors.New("legacy Remote Docker process did not stop")
		case <-timer.C:
		}
	}
}

func configureDesktopShell(platform string, setAccessory func() error) error {
	if platform != "darwin" {
		return nil
	}
	if setAccessory == nil {
		return errors.New("configure macOS accessory application")
	}
	return setAccessory()
}

func completeDesktopShutdown(
	stopRuntime func(context.Context) error,
	closeLocalAPI func() error,
	stopShell func(context.Context) error,
) error {
	runtimeCtx, cancelRuntime := context.WithTimeout(context.Background(), 30*time.Second)
	runtimeErr := stopRuntime(runtimeCtx)
	cancelRuntime()
	localAPIErr := closeLocalAPI()
	if errors.Is(localAPIErr, net.ErrClosed) {
		localAPIErr = nil
	}
	shellCtx, cancelShell := context.WithTimeout(context.Background(), 5*time.Second)
	shellErr := stopShell(shellCtx)
	cancelShell()
	return errors.Join(runtimeErr, localAPIErr, shellErr)
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
	show     func() error
	shutdown func()
}

func (h *desktopAPIHandler) setShutdown(shutdown func()) {
	h.mu.Lock()
	h.shutdown = shutdown
	h.mu.Unlock()
}

func (h *desktopAPIHandler) setShow(show func() error) {
	h.mu.Lock()
	h.show = show
	h.mu.Unlock()
}

func (h *desktopAPIHandler) Handle(ctx context.Context, method localapi.Method, params json.RawMessage) (any, error) {
	if method == localapi.MethodShowWindow {
		h.mu.RLock()
		show := h.show
		h.mu.RUnlock()
		if show == nil {
			return nil, errors.New("desktop application is not ready")
		}
		if err := show(); err != nil {
			return nil, err
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

type showWindowResult struct {
	Shown bool `json:"shown"`
}

type localAPICall func(context.Context, localapi.Method, any, any) error

func showExistingDesktop(ctx context.Context, timeout time.Duration, call localAPICall) error {
	if call == nil {
		return errors.New("show existing Remote Docker window is unavailable")
	}
	if timeout <= 0 {
		return errors.New("show existing Remote Docker window timeout is invalid")
	}
	focusCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var result showWindowResult
	if err := call(focusCtx, localapi.MethodShowWindow, nil, &result); err != nil {
		return fmt.Errorf("show existing Remote Docker window: %w", err)
	}
	if !result.Shown {
		return errors.New("existing Remote Docker window was not shown")
	}
	return nil
}
