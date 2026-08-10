package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	wails "github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/Dmitbd/remote-docker/internal/desktopui"
	"github.com/Dmitbd/remote-docker/internal/localapi"
)

const (
	bridgeCallTimeout = 20 * time.Second
	quitTimeout       = 32 * time.Second
)

type uiBackend interface {
	Snapshot(context.Context) (desktopui.State, error)
	Perform(context.Context, string, string) (desktopui.State, error)
	Quit(context.Context) error
}

type UIBridge struct {
	mu      sync.RWMutex
	ctx     context.Context
	backend uiBackend
	focus   net.Listener
}

func (b *UIBridge) startup(ctx context.Context) {
	b.mu.Lock()
	b.ctx = ctx
	b.mu.Unlock()
	b.startFocusServer(ctx)
}

func (b *UIBridge) shutdown(context.Context) {
	b.mu.Lock()
	listener := b.focus
	b.focus = nil
	b.ctx = nil
	b.mu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
}

func (b *UIBridge) Snapshot() (desktopui.State, error) {
	ctx, cancel, err := b.callContext(bridgeCallTimeout)
	if err != nil {
		return desktopui.State{}, err
	}
	defer cancel()
	return b.backend.Snapshot(ctx)
}

func (b *UIBridge) Perform(id, value string) (desktopui.State, error) {
	ctx, cancel, err := b.callContext(operationTimeout(id))
	if err != nil {
		return desktopui.State{}, err
	}
	defer cancel()
	return b.backend.Perform(ctx, id, value)
}

func (b *UIBridge) PickWorkspace() (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("папки проектов выбираются на Mac")
	}
	ctx, cancel, err := b.callContext(5 * time.Minute)
	if err != nil {
		return "", err
	}
	defer cancel()
	path, err := wailsruntime.OpenDirectoryDialog(ctx, wailsruntime.OpenDialogOptions{
		Title: "Выберите папку проекта",
	})
	if err != nil {
		return "", errors.New("не удалось открыть выбор папки")
	}
	return path, nil
}

func (b *UIBridge) Quit() error {
	ctx, cancel, err := b.callContext(quitTimeout)
	if err != nil {
		return err
	}
	defer cancel()
	if err := b.backend.Quit(ctx); err != nil {
		return err
	}
	wailsruntime.Quit(ctx)
	return nil
}

func (b *UIBridge) callContext(timeout time.Duration) (context.Context, context.CancelFunc, error) {
	if b == nil || b.backend == nil {
		return nil, nil, errors.New("окно Remote Docker не подключено к фоновому приложению")
	}
	b.mu.RLock()
	parent := b.ctx
	b.mu.RUnlock()
	if parent == nil {
		return nil, nil, errors.New("окно Remote Docker ещё не готово")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	return ctx, cancel, nil
}

func (b *UIBridge) startFocusServer(ctx context.Context) {
	endpoint, err := desktopui.FocusEndpoint()
	if err != nil {
		return
	}
	listener, err := localapi.Listen(endpoint)
	if err != nil {
		return
	}
	b.mu.Lock()
	b.focus = listener
	b.mu.Unlock()
	handler := localapi.HandlerFunc(func(_ context.Context, method localapi.Method, _ json.RawMessage) (any, error) {
		if method != localapi.MethodShowWindow {
			return nil, &localapi.PublicError{Code: localapi.ErrorInvalidRequest, Message: "UI focus method is unavailable"}
		}
		b.show()
		return map[string]bool{"shown": true}, nil
	})
	go func() { _ = (localapi.Server{Handler: handler}).Serve(ctx, listener) }()
}

func (b *UIBridge) show() {
	b.mu.RLock()
	ctx := b.ctx
	b.mu.RUnlock()
	if ctx == nil {
		return
	}
	wailsruntime.WindowUnminimise(ctx)
	wailsruntime.WindowShow(ctx)
}

func operationTimeout(id string) time.Duration {
	switch id {
	case desktopui.OperationApprovePair, desktopui.OperationRejectPair, desktopui.OperationCancelPair:
		return 90 * time.Second
	case desktopui.OperationAddProject:
		return 2 * time.Minute
	case desktopui.OperationQuit:
		return quitTimeout
	default:
		return bridgeCallTimeout
	}
}

func main() {
	if err := runUI(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Remote Docker UI could not start.")
		os.Exit(1)
	}
}

func runUI(args []string) error {
	backend, mocked, err := mockBackendFromArgs(args, runtime.GOOS)
	if err != nil {
		return err
	}
	if !mocked {
		backend = desktopui.NewBackend(localapi.Client{}, runtime.GOOS)
	}
	bridge := &UIBridge{backend: backend}
	return wails.Run(windowOptions(bridge))
}

func windowOptions(bridge *UIBridge) *options.App {
	icon := applicationIcon()
	return &options.App{
		Title: "Remote Docker", Width: 1050, Height: 720, MinWidth: 760, MinHeight: 580,
		BackgroundColour: options.NewRGB(11, 13, 18),
		AssetServer:      &assetserver.Options{Assets: frontendAssets()},
		OnStartup:        bridge.startup,
		OnShutdown:       bridge.shutdown,
		Bind:             []interface{}{bridge},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "io.github.dmitbd.remote-docker.ui",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) {
				bridge.show()
			},
		},
		ErrorFormatter: func(err error) any {
			return err.Error()
		},
		EnableDefaultContextMenu:         false,
		EnableFraudulentWebsiteDetection: false,
		DragAndDrop:                      &options.DragAndDrop{DisableWebViewDrop: true},
		Mac: &mac.Options{
			Appearance:  mac.NSAppearanceNameDarkAqua,
			DisableZoom: true,
			About:       &mac.AboutInfo{Title: "Remote Docker", Message: "Защищённый Docker на Windows", Icon: icon},
		},
		Windows: &windows.Options{
			Theme: windows.Dark, DisablePinchZoom: true, IsZoomControlEnabled: false,
			Messages: &windows.Messages{
				InstallationRequired: "Для интерфейса Remote Docker требуется Microsoft Edge WebView2 Runtime.",
				UpdateRequired:       "Обновите Microsoft Edge WebView2 Runtime перед запуском Remote Docker.",
				MissingRequirements:  "Требуется WebView2",
				Webview2NotInstalled: "Microsoft Edge WebView2 Runtime не установлен.",
				Error:                "Remote Docker",
				FailedToInstall:      "WebView2 не установлен. Используйте официальный установщик Microsoft.",
				DownloadPage:         "Откройте официальную страницу Microsoft Edge WebView2: ",
				PressOKToInstall:     "Установите WebView2 и повторите запуск.",
				ContactAdmin:         "Обратитесь к администратору для установки WebView2.",
				InvalidFixedWebview2: "Указанная установка WebView2 недоступна.",
				WebView2ProcessCrash: "Процесс WebView2 завершился. Перезапустите Remote Docker.",
			},
		},
	}
}
