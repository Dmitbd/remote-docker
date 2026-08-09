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

	"fyne.io/fyne/v2"
	"github.com/Dmitbd/remote-docker/internal/app"
	productassets "github.com/Dmitbd/remote-docker/internal/assets"
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

	uiController := desktop.NewController(controller, machine.Snapshot)
	application, err := desktop.NewApplication(desktop.ApplicationOptions{
		Controller: uiController, Snapshot: machine.Snapshot, Updates: updates,
		Icon: fyne.NewStaticResource("remote-docker.png", productassets.AppIcon()),
		OnQuit: func() {
			cancel()
			_ = listener.Close()
		},
	})
	if err != nil {
		return err
	}
	handler.setShow(application.Show)
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, _ = controller.Handle(shutdownCtx, localapi.MethodShutdown, nil)
		shutdownCancel()
		application.Quit()
	}()
	application.Run(ctx)
	cancel()
	_ = listener.Close()
	select {
	case <-serveDone:
	case <-time.After(2 * time.Second):
	}
	return nil
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
	base localapi.Handler
	mu   sync.RWMutex
	show func()
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
	return h.base.Handle(ctx, method, params)
}

var _ localapi.Handler = (*desktopAPIHandler)(nil)
