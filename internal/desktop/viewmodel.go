package desktop

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
)

type Section string

const (
	SectionConnection  Section = "connection"
	SectionWorkspaces  Section = "workspaces"
	SectionDiagnostics Section = "diagnostics"
	SectionResources   Section = "resources"
)

type ActionID string

const (
	ActionEnableClient  ActionID = "enable-client"
	ActionEnableHost    ActionID = "enable-host"
	ActionStartSearch   ActionID = "start-search"
	ActionStopSearch    ActionID = "stop-search"
	ActionConnect       ActionID = "connect"
	ActionApprovePair   ActionID = "approve-pair"
	ActionRejectPair    ActionID = "reject-pair"
	ActionCancelPair    ActionID = "cancel-pair"
	ActionPause         ActionID = "pause"
	ActionDisconnect    ActionID = "disconnect"
	ActionForgetDevice  ActionID = "forget-device"
	ActionAddWorkspace  ActionID = "add-workspace"
	ActionDiagnostics   ActionID = "diagnostics"
	ActionQuit          ActionID = "quit"
)

type Action struct {
	ID          ActionID
	Label       string
	Enabled     bool
	Destructive bool
	Icon        string
}

type ViewModel struct {
	LocalName       string
	Role            string
	Status          string
	Headline        string
	Detail          string
	PeerName        string
	PairCode        string
	ConnectionCount string
	Latency         string
	Docker          string
	Sync            string
	Countdown       string
	Selected        Section
	Sections        []Section
	Actions         []Action
}

func BuildViewModel(snapshot lifecycle.Snapshot, selected Section, now time.Time) ViewModel {
	if selected == "" {
		selected = SectionConnection
	}
	model := ViewModel{
		LocalName: snapshot.LocalName, Selected: selected,
		Sections: []Section{SectionConnection, SectionWorkspaces, SectionDiagnostics, SectionResources},
		ConnectionCount: fmt.Sprintf("%d из %d", snapshot.TrustedPeers, maximum(snapshot.ConnectionLimit, 1)),
	}
	if snapshot.Role == lifecycle.RoleWindowsHost {
		model.Role = "Windows · запускает Docker"
	} else {
		model.Role = "Mac · отправляет Docker-команды"
	}
	if snapshot.Peer != nil {
		model.PeerName = snapshot.Peer.Name
	}

	switch snapshot.State {
	case lifecycle.StatePaused:
		model.Status = "На паузе"
		model.Headline = "Remote Docker выключен"
		model.Detail = "Фоновые процессы не запущены и ресурсы не используются."
		if snapshot.Role == lifecycle.RoleWindowsHost {
			model.Actions = append(model.Actions, enabledAction(ActionEnableHost, "Начать ожидание подключения"))
		} else {
			model.Actions = append(model.Actions, enabledAction(ActionEnableClient, "Включить Remote Docker"))
		}
	case lifecycle.StateClientReady:
		model.Status = "Готов к поиску"
		model.Headline = "Выберите Windows-компьютер"
		model.Detail = "Поиск запускается только по вашей команде."
		model.Actions = append(model.Actions, enabledAction(ActionStartSearch, "Найти компьютер"), enabledAction(ActionPause, "Поставить на паузу"))
	case lifecycle.StateSearching:
		model.Status = "Поиск устройств"
		model.Headline = "Ищем Windows-компьютер в этой сети"
		model.Detail = "Доступные компьютеры появятся здесь."
		model.Actions = append(model.Actions, enabledAction(ActionStopSearch, "Остановить поиск"), enabledAction(ActionPause, "Поставить на паузу"))
	case lifecycle.StateHostWaiting:
		model.Status = "Ожидает подключения"
		model.Headline = "Этот компьютер доступен для Mac"
		model.Detail = "Ожидание можно завершить, поставив приложение на паузу."
		model.Actions = append(model.Actions, enabledAction(ActionPause, "Поставить на паузу"))
	case lifecycle.StatePairing:
		model.Status = "Подтверждение устройства"
		model.Headline = "Сравните код на двух устройствах"
		if snapshot.Pairing != nil {
			model.PeerName = snapshot.Pairing.Peer.Name
			model.PairCode = displayPairCode(snapshot.Pairing.Code)
		}
		model.Detail = "Код вводить не нужно. Он должен совпадать на Mac и Windows."
		if snapshot.Role == lifecycle.RoleWindowsHost {
			model.Actions = append(model.Actions,
				enabledAction(ActionApprovePair, "Код совпадает — разрешить"),
				Action{ID: ActionRejectPair, Label: "Отклонить", Enabled: true, Destructive: true},
			)
		} else {
			model.Actions = append(model.Actions, Action{ID: ActionCancelPair, Label: "Отменить подключение", Enabled: true, Destructive: true})
		}
	case lifecycle.StateConnecting:
		model.Status = "Подключение"
		model.Headline = "Настраиваем защищённое соединение"
		model.Detail = "Docker и синхронизация запускаются на Windows-компьютере."
	case lifecycle.StateConnected:
		model.Status = "Соединено"
		model.Headline = "Docker работает на Windows-компьютере"
		model.Detail = "Команды Docker с Mac выполняются удалённо."
		model.Latency = fmt.Sprintf("%d мс", snapshot.Latency.Milliseconds())
		model.Docker = serviceLabel("Docker", snapshot.Docker.State)
		model.Sync = serviceLabel("Синхронизация", snapshot.Sync.State)
		model.Actions = append(model.Actions,
			enabledAction(ActionDisconnect, "Отключиться"),
			enabledAction(ActionPause, "Поставить на паузу"),
		)
	case lifecycle.StateReconnecting:
		model.Status = "Восстановление связи"
		model.Headline = "Соединение временно потеряно"
		model.Detail = "Пытаемся восстановить связь с тем же доверенным устройством."
		if snapshot.Recovery != nil {
			seconds := int(math.Ceil(snapshot.Recovery.Deadline.Sub(now).Seconds()))
			if seconds < 0 {
				seconds = 0
			}
			model.Countdown = fmt.Sprintf("%d сек", seconds)
		}
		model.Actions = append(model.Actions, enabledAction(ActionDisconnect, "Завершить соединение"))
	case lifecycle.StateStopping:
		model.Status = "Завершение работы"
		model.Headline = "Безопасно останавливаем процессы"
		model.Detail = "Дождитесь завершения очистки."
	case lifecycle.StateNeedsAction:
		model.Status = "Нужна помощь"
		model.Headline = "Remote Docker требует внимания"
		if snapshot.Problem != nil {
			model.Detail = snapshot.Problem.Message
		}
		model.Actions = append(model.Actions, enabledAction(ActionDiagnostics, "Открыть диагностику"))
	default:
		model.Status = "Неизвестное состояние"
		model.Headline = "Откройте диагностику"
	}

	model.Actions = append(model.Actions, Action{ID: ActionQuit, Label: "Завершить работу", Enabled: !snapshot.ActionInProgress, Destructive: true, Icon: "exit"})
	return model
}

func enabledAction(id ActionID, label string) Action {
	return Action{ID: id, Label: label, Enabled: true}
}

func displayPairCode(code string) string {
	if len(code) != 6 {
		return code
	}
	return code[:3] + " " + code[3:]
}

func serviceLabel(name string, state lifecycle.ServiceState) string {
	switch state {
	case lifecycle.ServiceReady:
		return name + " готов"
	case lifecycle.ServiceStarting:
		return name + " запускается"
	case lifecycle.ServiceError:
		return name + " требует внимания"
	default:
		return name + " остановлен"
	}
}

func maximum(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func sectionLabel(section Section) string {
	switch section {
	case SectionWorkspaces:
		return "Проекты"
	case SectionDiagnostics:
		return "Диагностика"
	case SectionResources:
		return "Нагрузка"
	default:
		return "Соединение"
	}
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
