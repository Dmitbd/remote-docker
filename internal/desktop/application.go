package desktop

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	fyneapp "fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	desktopdriver "fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	productassets "github.com/Dmitbd/remote-docker/internal/assets"
	"github.com/Dmitbd/remote-docker/internal/lifecycle"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/metrics"
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
	trayIcon   fyne.Resource
	onQuit     func()

	mu                   sync.Mutex
	selected             Section
	content              *fyne.Container
	status               *widget.Label
	role                 *widget.Label
	lastPairNotification string
	candidates           []localapi.PairingCandidate
	workspaces           []localapi.Workspace
	checks               []localapi.DoctorCheck
	resources            metrics.Sample
	lastDiagnosticsPoll  time.Time
	actionError          string
	actionSequence       uint64
	confirm              func(title, message, confirmText string, response func(bool))
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
	go a.poll(ctx)
	a.window.Show()
	a.app.Run()
}

func (a *Application) poll(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		a.pollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *Application) pollOnce(ctx context.Context) {
	snapshot := a.snapshot()
	switch snapshot.State {
	case lifecycle.StateClientReady, lifecycle.StateSearching:
		pollCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		candidates, err := a.controller.Candidates(pollCtx)
		cancel()
		if err == nil {
			a.mu.Lock()
			a.candidates = append([]localapi.PairingCandidate(nil), candidates...)
			a.mu.Unlock()
			fyne.Do(func() { a.render(a.snapshot()) })
		}
	case lifecycle.StateHostWaiting, lifecycle.StatePairing:
		pollCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		_, _ = a.controller.PollPairing(pollCtx)
		cancel()
	}
	a.mu.Lock()
	selected := a.selected
	lastDiagnosticsPoll := a.lastDiagnosticsPoll
	if selected == SectionDiagnostics && time.Since(lastDiagnosticsPoll) >= 10*time.Second {
		a.lastDiagnosticsPoll = time.Now()
	}
	a.mu.Unlock()
	if selected == SectionWorkspaces {
		pollCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		workspaces, err := a.controller.Workspaces(pollCtx)
		cancel()
		if err == nil {
			a.mu.Lock()
			a.workspaces = append([]localapi.Workspace(nil), workspaces...)
			a.mu.Unlock()
			fyne.Do(func() { a.render(a.snapshot()) })
		}
	} else if selected == SectionDiagnostics && time.Since(lastDiagnosticsPoll) >= 10*time.Second {
		pollCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		checks, err := a.controller.Diagnostics(pollCtx)
		cancel()
		if err == nil {
			a.mu.Lock()
			a.checks = append([]localapi.DoctorCheck(nil), checks...)
			a.mu.Unlock()
			fyne.Do(func() { a.render(a.snapshot()) })
		}
	} else if selected == SectionResources {
		pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resources, err := a.controller.Resources(pollCtx)
		cancel()
		if err == nil {
			a.mu.Lock()
			a.resources = resources
			a.mu.Unlock()
			fyne.Do(func() { a.render(a.snapshot()) })
		}
	}
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
			Title:   "Запрос на подключение",
			Content: "Откройте Remote Docker и сравните код на Mac и Windows.",
		})
	}
	model := BuildViewModel(snapshot, a.selected, time.Now())
	a.trayIcon = fyne.NewStaticResource("remote-docker-tray.png", productassets.TrayIcon(runtime.GOOS, productassets.TrayStateFor(snapshot)))
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

	var body *fyne.Container
	switch model.Selected {
	case SectionWorkspaces:
		body = a.workspaceBody()
	case SectionDiagnostics:
		body = a.diagnosticsBody()
	case SectionResources:
		body = a.resourcesBody()
	default:
		body = a.connectionBody(model, snapshot)
	}

	var quit Action
	for _, action := range model.Actions {
		if action.ID == ActionQuit {
			quit = action
		}
	}

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

func (a *Application) connectionBody(model ViewModel, snapshot lifecycle.Snapshot) *fyne.Container {
	headline := widget.NewLabel(model.Headline)
	headline.TextStyle = fyne.TextStyle{Bold: true}
	detail := widget.NewLabel(model.Detail)
	detail.Wrapping = fyne.TextWrapWord
	body := container.NewVBox(headline, detail)
	if a.actionError != "" {
		actionError := widget.NewLabel(a.actionError)
		actionError.TextStyle = fyne.TextStyle{Bold: true}
		body.Add(actionError)
	}
	if model.Notice != "" {
		notice := widget.NewLabel(model.Notice)
		notice.TextStyle = fyne.TextStyle{Bold: true}
		body.Add(notice)
	}
	if model.PeerName != "" {
		body.Add(widget.NewLabel("Устройство: " + model.PeerName))
	}
	if model.PairCode != "" {
		code := widget.NewLabel(model.PairCode)
		code.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}
		body.Add(code)
	}
	rows := BuildDeviceRows(snapshot, a.candidates)
	if len(rows) == 0 && snapshot.State == lifecycle.StateSearching {
		body.Add(widget.NewLabel("Пока ничего не найдено"))
	}
	for _, row := range rows {
		row := row
		name := widget.NewLabel(row.Name + " · " + row.Status)
		name.TextStyle = fyne.TextStyle{Bold: true}
		actions := container.NewHBox()
		for _, action := range row.Actions {
			action := action
			button := widget.NewButton(action.Label, func() { a.performRowAction(snapshot, row, action) })
			if !action.Enabled {
				button.Disable()
			}
			if action.Destructive {
				button.Importance = widget.DangerImportance
			} else {
				button.Importance = widget.HighImportance
			}
			actions.Add(button)
		}
		body.Add(container.NewBorder(nil, nil, name, actions))
	}
	if model.Countdown != "" {
		body.Add(widget.NewLabel("До безопасной остановки: " + model.Countdown))
	}
	metricRow := container.NewHBox()
	for _, metric := range []string{model.ConnectionCount, model.Latency, model.Docker, model.Sync} {
		if metric != "" {
			metricRow.Add(widget.NewLabel(metric))
		}
	}
	if len(metricRow.Objects) > 0 {
		body.Add(metricRow)
	}
	actions := container.NewHBox()
	for _, action := range model.Actions {
		if action.ID == ActionQuit {
			continue
		}
		action := action
		button := widget.NewButton(action.Label, func() { a.perform(action.ID, "") })
		if !action.Enabled {
			button.Disable()
		}
		if action.Destructive {
			button.Importance = widget.DangerImportance
		} else if action.ID == ActionApprovePair || action.ID == ActionStartSearch || action.ID == ActionEnableClient || action.ID == ActionEnableHost {
			button.Importance = widget.HighImportance
		}
		actions.Add(button)
	}
	body.Add(actions)
	return body
}

func (a *Application) workspaceBody() *fyne.Container {
	title := widget.NewLabel("Проекты")
	title.TextStyle = fyne.TextStyle{Bold: true}
	add := widget.NewButton("Добавить папку проекта", func() {
		picker := dialog.NewFolderOpen(func(folder fyne.ListableURI, err error) {
			if err != nil || folder == nil {
				return
			}
			a.perform(ActionAddWorkspace, folder.Path())
		}, a.window)
		picker.Show()
	})
	add.Importance = widget.HighImportance
	body := container.NewVBox(title, widget.NewLabel("Папки остаются на Mac, а Docker получает их синхронизированную копию на Windows."), add)
	if len(a.workspaces) == 0 {
		body.Add(widget.NewLabel("Проекты ещё не добавлены"))
	}
	for _, workspace := range a.workspaces {
		workspace := workspace
		remove := widget.NewButton("Удалить", func() {
			go func() { _ = a.controller.RemoveWorkspace(context.Background(), workspace.ID) }()
		})
		remove.Importance = widget.DangerImportance
		body.Add(container.NewBorder(nil, nil, widget.NewLabel(workspace.Path), remove))
	}
	return body
}

func (a *Application) diagnosticsBody() *fyne.Container {
	title := widget.NewLabel("Диагностика")
	title.TextStyle = fyne.TextStyle{Bold: true}
	body := container.NewVBox(title, widget.NewLabel("Здесь показаны только безопасные проверки Remote Docker."))
	if len(a.checks) == 0 {
		body.Add(widget.NewLabel("Проверки ещё не запускались"))
	}
	for _, check := range a.checks {
		state := "Нужна помощь"
		if check.OK {
			state = "Готово"
		}
		text := check.Name + " — " + state
		if check.Message != "" {
			text += ": " + check.Message
		}
		body.Add(widget.NewLabel(text))
	}
	return body
}

func (a *Application) perform(action ActionID, value string) {
	sequence := a.beginAction()
	a.refreshCurrent()
	go func() {
		err := a.controller.Perform(context.Background(), action, value)
		if !a.completeAction(sequence, err) {
			return
		}
		a.refreshCurrent()
		if err != nil {
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

func (a *Application) performRowAction(snapshot lifecycle.Snapshot, row DeviceRow, action Action) {
	switch action.ID {
	case ActionForgetDevice:
		a.showForgetConfirmation(row)
	case ActionConnect:
		if connectionLimitOccupied(snapshot) {
			a.showReplaceConfirmation(row)
			return
		}
		a.perform(action.ID, row.ID)
	default:
		a.perform(action.ID, row.ID)
	}
}

func (a *Application) showForgetConfirmation(row DeviceRow) {
	a.showConfirmation(
		"Забыть устройство?",
		"Связь с устройством «"+nonEmpty(row.Name, "Windows-компьютер")+"» будет удалена. Проекты, Docker-данные, WSL и исходники останутся без изменений.",
		"Забыть устройство",
		func(confirmed bool) {
			if confirmed {
				a.forgetDevice(row, false, nil)
			}
		},
	)
}

func (a *Application) showReplaceConfirmation(row DeviceRow) {
	snapshot := a.snapshot()
	if snapshot.Peer == nil {
		a.perform(ActionConnect, row.ID)
		return
	}
	a.showConfirmation(
		"Подключить новое устройство?",
		"Старая доверенная связь будет удалена перед подключением к «"+nonEmpty(row.Name, "новому Windows-компьютеру")+"». Проекты, Docker-данные, WSL и исходники останутся без изменений.",
		"Забыть старую связь и подключиться к новому устройству",
		func(confirmed bool) {
			if confirmed {
				a.forgetDevice(DeviceRow{ID: snapshot.Peer.ID, Name: snapshot.Peer.Name}, false, func() {
					a.perform(ActionConnect, row.ID)
				})
			}
		},
	)
}

func (a *Application) showLocalForgetConfirmation(row DeviceRow, after func()) {
	a.showConfirmation(
		"Забыть только на этом компьютере?",
		"Удалённый отзыв сейчас недоступен. Связь будет удалена только на этом компьютере. Проекты, Docker-данные, WSL и исходники останутся без изменений; на другом компьютере доверие может потребовать отдельного удаления.",
		"Забыть только здесь",
		func(confirmed bool) {
			if confirmed {
				a.forgetDevice(row, true, after)
			}
		},
	)
}

func (a *Application) forgetDevice(row DeviceRow, localOnly bool, after func()) {
	sequence := a.beginAction()
	a.refreshCurrent()
	go func() {
		err := a.controller.ForgetDevice(context.Background(), row.ID, localOnly)
		if !a.completeAction(sequence, err) {
			return
		}
		a.refreshCurrent()
		if err != nil {
			if !localOnly && remoteUnavailable(err) {
				a.offerLocalForgetConfirmation(row, after)
			}
			return
		}
		if after != nil {
			after()
		}
	}()
}

func (a *Application) offerLocalForgetConfirmation(row DeviceRow, after func()) {
	if a.confirm != nil {
		a.showLocalForgetConfirmation(row, after)
		return
	}
	fyne.Do(func() { a.showLocalForgetConfirmation(row, after) })
}

func (a *Application) showConfirmation(title, message, confirmText string, response func(bool)) {
	if a.confirm != nil {
		a.confirm(title, message, confirmText, response)
		return
	}
	if a.window == nil {
		return
	}
	confirmation := dialog.NewConfirm(title, message, response, a.window)
	confirmation.SetConfirmText(confirmText)
	confirmation.SetDismissText("Отмена")
	confirmation.Show()
}

func (a *Application) refreshCurrent() {
	if a.app == nil || a.content == nil {
		return
	}
	fyne.Do(func() { a.render(a.snapshot()) })
}

func (a *Application) beginAction() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.actionSequence++
	a.actionError = ""
	return a.actionSequence
}

func (a *Application) completeAction(sequence uint64, err error) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if sequence != a.actionSequence {
		return false
	}
	if err != nil {
		a.actionError = safeActionMessage(err)
	} else {
		a.actionError = ""
	}
	return true
}

func safeActionMessage(err error) string {
	if errors.Is(err, ErrConnectionLimit) {
		return "Можно сохранить только одно доверенное устройство. Сначала забудьте сохранённый компьютер."
	}
	var public *localapi.PublicError
	if errors.As(err, &public) {
		return safeLocalAPIErrorMessage(public.Code)
	}
	var remote *localapi.RemoteError
	if errors.As(err, &remote) {
		return safeLocalAPIErrorMessage(remote.Code)
	}
	return "Не удалось выполнить действие. Попробуйте снова."
}

func remoteUnavailable(err error) bool {
	var public *localapi.PublicError
	if errors.As(err, &public) && public.Code == localapi.ErrorUnavailable {
		return true
	}
	var remote *localapi.RemoteError
	return errors.As(err, &remote) && remote.Code == localapi.ErrorUnavailable
}

func safeLocalAPIErrorMessage(code localapi.ErrorCode) string {
	switch code {
	case localapi.ErrorNeedsAction:
		return "Действие сейчас недоступно. Проверьте состояние подключения."
	case localapi.ErrorInvalidRequest:
		return "Не удалось выполнить действие. Проверьте выбранное устройство."
	case localapi.ErrorPeerForbidden:
		return "Подключение отклонено. Проверьте, что это доверенное устройство."
	case localapi.ErrorUnavailable:
		return "Remote Docker сейчас недоступен. Попробуйте снова."
	default:
		return "Не удалось выполнить действие. Попробуйте снова."
	}
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
	if a.trayIcon != nil {
		desktopApp.SetSystemTrayIcon(a.trayIcon)
	} else if a.icon != nil {
		desktopApp.SetSystemTrayIcon(a.icon)
	}
	desktopApp.SetSystemTrayWindow(a.window)
}
