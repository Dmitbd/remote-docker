package desktop

import (
	"context"
	"errors"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	fyneapp "fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	desktopdriver "fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/Dmitbd/remote-docker/internal/lifecycle"
)

const applicationID = "io.github.dmitbd.remote-docker"

type ApplicationOptions struct {
	App        fyne.App
	Controller *Controller
	Snapshot   SnapshotProvider
	Updates    <-chan lifecycle.Snapshot
	Icon       fyne.Resource
	OnQuit     func()
}

type Application struct {
	app        fyne.App
	window     fyne.Window
	controller *Controller
	snapshot   SnapshotProvider
	updates    <-chan lifecycle.Snapshot
	icon       fyne.Resource
	onQuit     func()

	mu       sync.Mutex
	selected Section
	content  *fyne.Container
	status   *widget.Label
	role     *widget.Label
	lastPairNotification string
}

func NewApplication(options ApplicationOptions) (*Application, error) {
	if options.Controller == nil || options.Snapshot == nil {
		return nil, errors.New("desktop application dependencies are incomplete")
	}
	application := options.App
	if application == nil {
		application = fyneapp.NewWithID(applicationID)
	}
	application.Settings().SetTheme(newProductTheme())
	if options.Icon != nil {
		application.SetIcon(options.Icon)
	}
	window := application.NewWindow("Remote Docker")
	window.Resize(fyne.NewSize(860, 620))
	result := &Application{
		app: application, window: window, controller: options.Controller, snapshot: options.Snapshot,
		updates: options.Updates, icon: options.Icon, onQuit: options.OnQuit, selected: SectionConnection,
		content: container.NewVBox(), status: widget.NewLabel(""), role: widget.NewLabel(""),
	}
	window.SetCloseIntercept(func() { window.Hide() })
	result.render(options.Snapshot())
	result.configureTray()
	return result, nil
}

func (a *Application) Run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	go a.observe(ctx)
	a.window.Show()
	a.app.Run()
}

func (a *Application) Show() {
	fyne.Do(func() {
		a.window.Show()
		a.window.RequestFocus()
	})
}

func (a *Application) Quit() {
	fyne.Do(func() { a.app.Quit() })
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
			value := snapshot.Clone()
			fyne.Do(func() { a.render(value) })
		}
	}
}

func (a *Application) render(snapshot lifecycle.Snapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if snapshot.Role == lifecycle.RoleWindowsHost && snapshot.Pairing != nil &&
		snapshot.Pairing.Status == lifecycle.PairingPending && snapshot.Pairing.SessionID != a.lastPairNotification {
		a.lastPairNotification = snapshot.Pairing.SessionID
		a.app.SendNotification(&fyne.Notification{
			Title: "Запрос на подключение",
			Content: "Откройте Remote Docker и сравните код на Mac и Windows.",
		})
	}
	model := BuildViewModel(snapshot, a.selected, time.Now())
	a.status.SetText(model.Status)
	a.status.TextStyle = fyne.TextStyle{Bold: true}
	a.role.SetText(model.Role)

	deviceName := widget.NewLabel(nonEmpty(model.LocalName, "Это устройство"))
	deviceName.TextStyle = fyne.TextStyle{Bold: true}
	header := container.NewBorder(nil, nil, nil, container.NewVBox(a.status, a.role), deviceName)

	navigation := container.NewHBox()
	for _, section := range model.Sections {
		section := section
		button := widget.NewButton(sectionLabel(section), func() {
			a.mu.Lock()
			a.selected = section
			a.mu.Unlock()
			a.render(a.snapshot())
		})
		if section == model.Selected {
			button.Importance = widget.HighImportance
		}
		navigation.Add(button)
	}

	headline := widget.NewLabel(model.Headline)
	headline.TextStyle = fyne.TextStyle{Bold: true}
	detail := widget.NewLabel(model.Detail)
	detail.Wrapping = fyne.TextWrapWord
	body := container.NewVBox(headline, detail)
	if model.PeerName != "" {
		body.Add(widget.NewLabel("Устройство: " + model.PeerName))
	}
	if model.PairCode != "" {
		code := widget.NewLabel(model.PairCode)
		code.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
		body.Add(code)
	}
	if model.Countdown != "" {
		body.Add(widget.NewLabel("До безопасной остановки: " + model.Countdown))
	}
	metrics := []string{model.ConnectionCount, model.Latency, model.Docker, model.Sync}
	metricRow := container.NewHBox()
	for _, metric := range metrics {
		if metric != "" {
			metricRow.Add(widget.NewLabel(metric))
		}
	}
	if len(metricRow.Objects) > 0 {
		body.Add(metricRow)
	}

	actions := container.NewHBox()
	var quit Action
	for _, action := range model.Actions {
		if action.ID == ActionQuit {
			quit = action
			continue
		}
		action := action
		button := widget.NewButton(action.Label, func() { a.perform(action.ID, "") })
		button.Disable()
		if action.Enabled {
			button.Enable()
		}
		if action.Destructive {
			button.Importance = widget.DangerImportance
		} else if action.ID == ActionApprovePair || action.ID == ActionStartSearch || action.ID == ActionEnableClient || action.ID == ActionEnableHost {
			button.Importance = widget.HighImportance
		}
		actions.Add(button)
	}
	body.Add(actions)

	quitButton := widget.NewButtonWithIcon(quit.Label, theme.LogoutIcon(), func() { a.perform(ActionQuit, "") })
	quitButton.Importance = widget.DangerImportance
	if !quit.Enabled {
		quitButton.Disable()
	}
	footer := container.NewBorder(nil, nil, nil, quitButton, layout.NewSpacer())
	a.content.Objects = []fyne.CanvasObject{container.NewBorder(header, footer, nil, nil, container.NewVBox(navigation, widget.NewSeparator(), body))}
	a.content.Refresh()
	a.window.SetContent(a.content)
	a.configureTray()
}

func (a *Application) perform(action ActionID, value string) {
	go func() {
		if err := a.controller.Perform(context.Background(), action, value); err != nil {
			fyne.Do(func() { a.status.SetText("Нужна помощь") })
			return
		}
		if action == ActionQuit {
			if a.onQuit != nil {
				a.onQuit()
			}
			fyne.Do(func() { a.app.Quit() })
		}
	}()
}

func (a *Application) configureTray() {
	desktopApp, ok := a.app.(desktopdriver.App)
	if !ok {
		return
	}
	menu := fyne.NewMenu("Remote Docker",
		fyne.NewMenuItem("Открыть Remote Docker", a.Show),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Поставить на паузу", func() { a.perform(ActionPause, "") }),
		fyne.NewMenuItem("Завершить работу", func() { a.perform(ActionQuit, "") }),
	)
	desktopApp.SetSystemTrayMenu(menu)
	if a.icon != nil {
		desktopApp.SetSystemTrayIcon(a.icon)
	}
	desktopApp.SetSystemTrayWindow(a.window)
}
