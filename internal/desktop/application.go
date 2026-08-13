package desktop

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"

	"fyne.io/systray"

	productassets "github.com/Dmitbd/remote-docker/internal/assets"
	"github.com/Dmitbd/remote-docker/internal/lifecycle"
)

type trayMenuItem interface {
	Clicked() <-chan struct{}
}

type trayRuntime interface {
	Run(func(), func())
	Quit()
	SetRegularIcon([]byte)
	SetTemplateIcon([]byte)
	SetTooltip(string)
	AddMenuItem(string, string) trayMenuItem
	AddSeparator()
}

type ApplicationOptions struct {
	UI       UIProcess
	Snapshot SnapshotProvider
	Updates  <-chan lifecycle.Snapshot
	Platform string
	Tray     trayRuntime
	OnPause  func(context.Context) error
	OnQuit   func(context.Context) error
}

type Application struct {
	ui       UIProcess
	snapshot SnapshotProvider
	updates  <-chan lifecycle.Snapshot
	platform string
	tray     trayRuntime
	onPause  func(context.Context) error
	onQuit   func(context.Context) error
	quitOnce sync.Once
	quitErr  error
}

func NewApplication(options ApplicationOptions) (*Application, error) {
	if options.UI == nil || options.Snapshot == nil {
		return nil, errors.New("desktop application dependencies are incomplete")
	}
	platform := options.Platform
	if platform == "" {
		platform = runtime.GOOS
	}
	tray := options.Tray
	if tray == nil {
		tray = systemTray{}
	}
	return &Application{
		ui: options.UI, snapshot: options.Snapshot, updates: options.Updates,
		platform: platform, tray: tray, onPause: options.OnPause, onQuit: options.OnQuit,
	}, nil
}

func (a *Application) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := a.ui.Show(ctx); err != nil {
		return err
	}
	go a.observe(ctx)
	a.tray.Run(func() { a.ready(ctx) }, func() {})
	return a.quitErr
}

func (a *Application) Show() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return a.ui.Show(ctx)
}

func (a *Application) Quit(ctx context.Context) error {
	a.quitOnce.Do(func() {
		a.quitErr = a.ui.Stop(ctx)
		a.tray.Quit()
	})
	return a.quitErr
}

func (a *Application) ready(ctx context.Context) {
	a.updateTray(a.snapshot())
	open := a.tray.AddMenuItem("Открыть Remote Docker", "Показать окно Remote Docker")
	a.tray.AddSeparator()
	pause := a.tray.AddMenuItem("Поставить на паузу", "Остановить сетевую активность Remote Docker")
	quit := a.tray.AddMenuItem("Завершить работу", "Полностью остановить Remote Docker")
	go a.handleOpen(ctx, open)
	go a.handleAction(ctx, pause, a.onPause)
	go a.handleAction(ctx, quit, a.onQuit)
}

func (a *Application) handleOpen(ctx context.Context, item trayMenuItem) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-item.Clicked():
			_ = a.Show()
		}
	}
}

func (a *Application) handleAction(ctx context.Context, item trayMenuItem, action func(context.Context) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-item.Clicked():
			if action == nil {
				continue
			}
			actionCtx, cancel := context.WithTimeout(context.Background(), 32*time.Second)
			_ = action(actionCtx)
			cancel()
		}
	}
}

func (a *Application) observe(ctx context.Context) {
	if a.updates == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case snapshot, ok := <-a.updates:
			if !ok {
				return
			}
			a.updateTray(snapshot)
		}
	}
}

func (a *Application) updateTray(snapshot lifecycle.Snapshot) {
	icon := productassets.TrayIcon(a.platform, productassets.TrayStateFor(snapshot))
	if a.platform == "darwin" {
		a.tray.SetTemplateIcon(icon)
	} else {
		a.tray.SetRegularIcon(icon)
	}
	a.tray.SetTooltip("Remote Docker · " + string(snapshot.State))
}

type systemTray struct{}

func (systemTray) Run(ready, exit func())      { systray.Run(ready, exit) }
func (systemTray) Quit()                       { systray.Quit() }
func (systemTray) SetRegularIcon(icon []byte)  { systray.SetIcon(icon) }
func (systemTray) SetTemplateIcon(icon []byte) { systray.SetTemplateIcon(icon, icon) }
func (systemTray) SetTooltip(value string)     { systray.SetTooltip(value) }
func (systemTray) AddMenuItem(title, tooltip string) trayMenuItem {
	return systemTrayItem{item: systray.AddMenuItem(title, tooltip)}
}
func (systemTray) AddSeparator() { systray.AddSeparator() }

type systemTrayItem struct{ item *systray.MenuItem }

func (i systemTrayItem) Clicked() <-chan struct{} { return i.item.ClickedCh }
