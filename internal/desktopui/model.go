// Package desktopui defines the process-independent contract between the
// lifecycle owner and the Wails window. It deliberately contains no Wails or
// Fyne types.
package desktopui

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/metrics"
)

const (
	OperationEnableClient   = "enable-client"
	OperationEnableHost     = "enable-host"
	OperationStartSearch    = "start-search"
	OperationStopSearch     = "stop-search"
	OperationConnect        = "connect"
	OperationConnectTrusted = "connect-trusted"
	OperationApprovePair    = "approve-pair"
	OperationRejectPair     = "reject-pair"
	OperationCancelPair     = "cancel-pair"
	OperationPause          = "pause"
	OperationDisconnect     = "disconnect"
	OperationForgetDevice   = "forget-device"
	OperationAddProject     = "add-project"
	OperationRemoveProject  = "remove-project"
	OperationDiagnostics    = "diagnostics"
	OperationManualAddress  = "manual-address"
	OperationQuit           = "quit"
)

const (
	DiagnosticReady       = "ready"
	DiagnosticRunning     = "running"
	DiagnosticNeedsHelp   = "needs_help"
	DiagnosticUnavailable = "unavailable"
)

type Operation struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	PendingLabel string `json:"pendingLabel"`
	Enabled      bool   `json:"enabled"`
	Destructive  bool   `json:"destructive"`
	Pending      bool   `json:"pending"`
}

type Device struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Role       string      `json:"role"`
	Status     string      `json:"status"`
	Kind       string      `json:"kind"`
	Trusted    bool        `json:"trusted"`
	Available  bool        `json:"available"`
	Operations []Operation `json:"operations"`
}

type Connection struct {
	Status               string `json:"status"`
	Tone                 string `json:"tone"`
	Headline             string `json:"headline"`
	Detail               string `json:"detail"`
	PeerName             string `json:"peerName"`
	PairCode             string `json:"pairCode"`
	Notice               string `json:"notice"`
	Countdown            string `json:"countdown"`
	Latency              string `json:"latency"`
	Docker               string `json:"docker"`
	Sync                 string `json:"sync"`
	AutomaticReconnect   bool   `json:"automaticReconnect"`
	DisconnectedBy       string `json:"disconnectedBy"`
	ManualAddressVisible bool   `json:"manualAddressVisible"`
}

type Project struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Path        string      `json:"path"`
	SyncStatus  string      `json:"syncStatus"`
	LastSuccess string      `json:"lastSuccess"`
	Error       string      `json:"error"`
	Operations  []Operation `json:"operations"`
}

type ResourceCard struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Subtitle  string `json:"subtitle"`
	Value     string `json:"value"`
	Detail    string `json:"detail"`
	Available bool   `json:"available"`
}

type Resources struct {
	UpdatedAt string         `json:"updatedAt"`
	Cards     []ResourceCard `json:"cards"`
}

type Diagnostic struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Status      string `json:"status"`
	StatusLabel string `json:"statusLabel"`
	Detail      string `json:"detail"`
	Action      string `json:"action"`
}

type State struct {
	Revision        uint64       `json:"revision"`
	Platform        string       `json:"platform"`
	Role            string       `json:"role"`
	Lifecycle       string       `json:"lifecycle"`
	SelectedSection string       `json:"selectedSection"`
	LocalName       string       `json:"localName"`
	PairSessionID   string       `json:"pairSessionId"`
	Devices         []Device     `json:"devices"`
	Operations      []Operation  `json:"operations"`
	Connection      Connection   `json:"connection"`
	Projects        []Project    `json:"projects"`
	Resources       Resources    `json:"resources"`
	Diagnostics     []Diagnostic `json:"diagnostics"`
	Error           string       `json:"error"`
}

type SnapshotInput struct {
	Status      localapi.StatusResult
	Candidates  []localapi.PairingCandidate
	Workspaces  []localapi.Workspace
	Sync        localapi.SyncStatusResult
	Resources   localapi.ResourceStatusResult
	Diagnostics []localapi.DoctorCheck
	PendingID   string
}

func BuildState(input SnapshotInput, platform string, now time.Time) State {
	status := input.Status
	state := State{
		Revision: status.Revision, Platform: platform, Lifecycle: status.State,
		SelectedSection: "connection", LocalName: fallback(status.LocalName, "Это устройство"),
	}
	if status.Role == "windows_host" {
		state.Role = "Windows · запускает Docker"
	} else {
		state.Role = "Mac · отправляет Docker-команды"
	}
	if status.Pairing != nil {
		state.PairSessionID = status.Pairing.SessionID
	}
	state.Connection, state.Operations = buildConnection(status, now)
	state.Devices = buildDevices(status, input.Candidates)
	state.Projects = buildProjects(input.Workspaces, input.Sync, status.Sync)
	state.Resources = buildResources(input.Resources)
	state.Diagnostics = buildDiagnostics(input.Diagnostics)
	applyPending(&state, input.PendingID, status.ActionInProgress)
	return state
}

func buildConnection(status localapi.StatusResult, now time.Time) (Connection, []Operation) {
	connection := Connection{Tone: "blue"}
	if status.Peer != nil {
		connection.PeerName = status.Peer.Name
	}
	if status.LastDisconnect != nil {
		connection.DisconnectedBy, connection.Notice = disconnectDetails(status.Role, *status.LastDisconnect)
	}
	var operations []Operation
	switch status.State {
	case "paused":
		connection.Status = "Выключено"
		connection.Tone = "gray"
		connection.Headline = "Remote Docker выключен"
		connection.Detail = "Фоновые процессы остановлены. Запуск выполняется только вручную."
		if status.Role == "windows_host" {
			operations = append(operations, operation(OperationEnableHost, true))
		} else {
			operations = append(operations, operation(OperationEnableClient, true))
		}
	case "client_ready":
		connection.Status = "Готов к поиску"
		connection.Headline = "Выберите Windows-компьютер"
		connection.Detail = "Поиск не запускается автоматически. Начните его, когда Windows готова принимать подключение."
		operations = append(operations, operation(OperationStartSearch, true), operation(OperationPause, true))
	case "searching":
		connection.Status = "Поиск устройств"
		connection.Headline = "Компьютеры в локальной сети"
		connection.Detail = "Выберите один Windows-компьютер. Одновременно можно подключиться только к одному устройству."
		operations = append(operations, operation(OperationStopSearch, true), operation(OperationPause, true))
	case "host_waiting":
		connection.Status = "Ожидает подключения"
		connection.Headline = "Этот компьютер доступен для Mac"
		connection.Detail = "Remote Docker виден только устройствам в этой локальной сети."
		operations = append(operations, operation(OperationPause, true))
	case "pairing":
		connection.Status = "Подтверждение устройства"
		connection.Headline = "Сравните код на двух устройствах"
		connection.Detail = "Код вводить не нужно. Он должен полностью совпадать на Mac и Windows."
		if status.Pairing != nil {
			connection.PeerName = status.Pairing.Peer.Name
			connection.PairCode = formatPairCode(status.Pairing.Code)
		}
		if status.Role == "windows_host" {
			operations = append(operations, operation(OperationApprovePair, true), operation(OperationRejectPair, true))
		} else {
			operations = append(operations, operation(OperationCancelPair, true))
		}
	case "pairing_cancellation_pending":
		connection.Status = "Отмена нового подключения"
		connection.Tone = "yellow"
		connection.Headline = "Завершите отмену нового сопряжения"
		connection.Detail = "Старое доверенное устройство сохранено. Повторите отмену нового подключения."
		operations = append(operations, operation(OperationCancelPair, true))
	case "connecting":
		connection.Status = "Подключение"
		connection.Headline = "Настраиваем защищённое соединение"
		connection.Detail = "Подготавливаем защищённый туннель, Docker и синхронизацию."
	case "connected":
		connection.Status = "Соединено"
		connection.Tone = "green"
		if status.Role == "windows_host" {
			connection.Headline = "Docker работает для Mac"
			connection.Detail = "Контейнеры, образы, тома и кеш используют ресурсы этого Windows-компьютера."
		} else {
			connection.Headline = "Docker работает на Windows"
			connection.Detail = "Команды Docker с этого Mac выполняются на подключённом компьютере."
		}
		if status.LatencyMS > 0 {
			connection.Latency = fmt.Sprintf("%d мс", status.LatencyMS)
		}
		connection.Docker = serviceStatus("Docker", status.Docker)
		connection.Sync = serviceStatus("Синхронизация", status.Sync)
		operations = append(operations, operation(OperationDisconnect, true), operation(OperationPause, true))
	case "reconnecting":
		connection.Status = "Восстановление связи"
		connection.Tone = "red"
		connection.Headline = "Соединение временно потеряно"
		connection.Detail = "Remote Docker автоматически пытается восстановить связь с тем же доверенным устройством."
		connection.AutomaticReconnect = true
		if status.Recovery != nil {
			if deadline, err := time.Parse(time.RFC3339Nano, status.Recovery.Deadline); err == nil {
				seconds := int(math.Ceil(deadline.Sub(now).Seconds()))
				if seconds < 0 {
					seconds = 0
				}
				connection.Countdown = fmt.Sprintf("%d сек", seconds)
			}
		}
		operations = append(operations, operation(OperationDisconnect, true))
	case "stopping":
		connection.Status = "Завершение работы"
		connection.Tone = "yellow"
		connection.Headline = "Безопасно останавливаем процессы"
		connection.Detail = "Новые действия отключены до завершения очистки."
	case "needs_action":
		connection.Status = "Нужна помощь"
		connection.Tone = "red"
		connection.Headline = "Remote Docker требует внимания"
		connection.Detail = "Откройте диагностику, чтобы увидеть безопасное следующее действие."
		if status.Problem != nil {
			connection.Detail = fallback(status.Problem.Message, connection.Detail)
			connection.Notice = status.Problem.Action
		}
		operations = append(operations, operation(OperationDiagnostics, true))
	default:
		connection.Status = "Недоступно"
		connection.Tone = "gray"
		connection.Headline = "Состояние приложения недоступно"
		connection.Detail = "Повторите попытку или откройте диагностику."
		operations = append(operations, operation(OperationDiagnostics, true))
	}
	if status.State != "stopping" && !status.Terminal {
		operations = append(operations, operation(OperationQuit, true))
	}
	return connection, operations
}

func buildDevices(status localapi.StatusResult, candidates []localapi.PairingCandidate) []Device {
	devices := make([]Device, 0, len(candidates)+1)
	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		id := strings.TrimSpace(candidate.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if status.Peer != nil && status.Peer.ID == id {
			candidate.Trusted = true
			candidate.Name = fallback(status.Peer.Name, candidate.Name)
		}
		devices = append(devices, buildDevice(status, candidate))
	}
	if status.Peer != nil && strings.TrimSpace(status.Peer.ID) != "" && !seen[status.Peer.ID] {
		devices = append(devices, buildDevice(status, localapi.PairingCandidate{
			ID: status.Peer.ID, Name: status.Peer.Name, Trusted: true,
		}))
	}
	sort.SliceStable(devices, func(i, j int) bool {
		if devices[i].Trusted != devices[j].Trusted {
			return devices[i].Trusted
		}
		if devices[i].Name == devices[j].Name {
			return devices[i].ID < devices[j].ID
		}
		return devices[i].Name < devices[j].Name
	})
	return devices
}

func buildDevice(status localapi.StatusResult, candidate localapi.PairingCandidate) Device {
	role := "Windows-хост"
	if status.Role == "windows_host" {
		role = "Mac-клиент"
	}
	device := Device{
		ID: candidate.ID, Name: fallback(candidate.Name, "Устройство"), Role: role,
		Trusted: candidate.Trusted, Available: candidate.Available,
	}
	active := status.Peer != nil && status.Peer.ID == candidate.ID
	if active {
		switch status.State {
		case "connected":
			device.Status, device.Kind = "Соединено", "connected"
			device.Available = true
			device.Operations = []Operation{operation(OperationDisconnect, true)}
			return device
		case "connecting":
			device.Status, device.Kind = "Подключение", "active"
			return device
		case "reconnecting":
			device.Status, device.Kind = "Восстановление связи", "active"
			device.Operations = []Operation{operation(OperationDisconnect, true)}
			return device
		case "pairing", "pairing_cancellation_pending", "stopping":
			device.Status, device.Kind = "Выполняется защищённое подключение", "active"
			return device
		}
	}
	canSelect := status.Role == "mac_client" && (status.State == "client_ready" || status.State == "searching")
	if !candidate.Trusted {
		device.Status, device.Kind = "Новое устройство", "new"
		device.Operations = []Operation{operation(OperationConnect, canSelect && status.State == "searching")}
		return device
	}
	device.Kind = "saved"
	if candidate.Available {
		device.Status = "Сохранено · доступно"
	} else {
		device.Status = "Сохранено · недоступно"
	}
	device.Operations = []Operation{
		operation(OperationConnectTrusted, canSelect && candidate.Available),
		operation(OperationForgetDevice, status.State != "connecting" && status.State != "reconnecting"),
	}
	return device
}

func buildProjects(workspaces []localapi.Workspace, syncResult localapi.SyncStatusResult, aggregate localapi.ServiceStatus) []Project {
	statuses := make(map[string]localapi.SyncFolderStatus, len(syncResult.Folders))
	for _, status := range syncResult.Folders {
		statuses[status.WorkspaceID] = status
	}
	projects := make([]Project, 0, len(workspaces))
	for _, workspace := range workspaces {
		folder := statuses[workspace.ID]
		project := Project{
			ID: workspace.ID, Name: fallback(workspace.Name, filepath.Base(workspace.Path)), Path: workspace.Path,
			SyncStatus: syncStatusLabel(fallback(folder.State, aggregate.State)),
			Operations: []Operation{operation(OperationRemoveProject, true)},
		}
		project.LastSuccess = formatLastSuccess(fallback(folder.LastSuccess, aggregate.LastSuccess))
		project.Error = safeProjectError(folder.Message)
		projects = append(projects, project)
	}
	return projects
}

func buildResources(sample metrics.Sample) Resources {
	resources := Resources{Cards: []ResourceCard{
		processCard("mac-app", "Remote Docker на Mac", "Приложение и локальные процессы", sample.MacRemoteDocker),
		rateCard("mac-sync", "Синхронизация на Mac", "Передача зарегистрированных исходников", sample.SyncNetwork),
		processCard("windows-app", "Remote Docker на Windows", "Хост приложения", sample.WindowsRemoteDocker),
		wslCard(sample),
	}}
	if !sample.At.IsZero() {
		resources.UpdatedAt = sample.At.Local().Format("15:04:05")
	}
	return resources
}

func processCard(id, title, subtitle string, usage metrics.ProcessUsage) ResourceCard {
	if !usage.CPUPercent.Available || !usage.MemoryBytes.Available {
		return ResourceCard{ID: id, Title: title, Subtitle: subtitle, Value: "Недоступно", Detail: safeUnavailableReason(usage.CPUPercent.Reason, usage.MemoryBytes.Reason)}
	}
	return ResourceCard{
		ID: id, Title: title, Subtitle: subtitle, Available: true,
		Value: fmt.Sprintf("%.1f%% CPU", usage.CPUPercent.Value), Detail: formatBytes(usage.MemoryBytes.Value),
	}
}

func rateCard(id, title, subtitle string, rate metrics.Rate) ResourceCard {
	if !rate.Available {
		return ResourceCard{ID: id, Title: title, Subtitle: subtitle, Value: "Недоступно", Detail: safeUnavailableReason(rate.Reason)}
	}
	return ResourceCard{ID: id, Title: title, Subtitle: subtitle, Available: true, Value: formatRate(rate.BytesPerSecond), Detail: "локальная сеть"}
}

func wslCard(sample metrics.Sample) ResourceCard {
	card := processCard("windows-wsl", "Управляемый WSL и Docker", "Docker Engine, контейнеры и данные", sample.WindowsManagedWSL)
	if card.Available && sample.DockerContainers.Available {
		card.Detail = fmt.Sprintf("%s · контейнеров: %d", card.Detail, sample.DockerContainers.Value)
	}
	return card
}

func buildDiagnostics(checks []localapi.DoctorCheck) []Diagnostic {
	result := make([]Diagnostic, 0, len(checks))
	for _, check := range checks {
		status := diagnosticStatus(check)
		result = append(result, Diagnostic{
			ID: check.Name, Label: diagnosticLabel(check.Name), Status: status,
			StatusLabel: diagnosticStatusLabel(status), Detail: diagnosticDetail(check.Name, check.Message),
			Action: safeDiagnosticAction(check.Action),
		})
	}
	return result
}

func diagnosticStatus(check localapi.DoctorCheck) string {
	switch strings.ToLower(strings.TrimSpace(check.Status)) {
	case "ready", "ok":
		return DiagnosticReady
	case "running", "checking", "pending":
		return DiagnosticRunning
	case "unavailable":
		return DiagnosticUnavailable
	case "needs_help", "failed", "error":
		return DiagnosticNeedsHelp
	}
	if check.OK {
		return DiagnosticReady
	}
	return DiagnosticNeedsHelp
}

func diagnosticLabel(id string) string {
	switch id {
	case "lan_reachability":
		return "Доступность Windows в локальной сети"
	case "tunnel_identity", "tls_identity":
		return "Доверие между устройствами"
	case "tunnel_session":
		return "Защищённый туннель"
	case "local_relays":
		return "Локальные сетевые маршруты"
	case "docker_channel":
		return "Docker Engine"
	case "sync_channel":
		return "Синхронизация проектов"
	case "managed_wsl":
		return "Управляемый WSL"
	default:
		return "Проверка Remote Docker"
	}
}

func diagnosticDetail(id, reason string) string {
	if strings.TrimSpace(reason) == "" {
		switch id {
		case "lan_reachability":
			return "Проверяется реальный защищённый путь между Mac и Windows."
		case "tunnel_identity", "tls_identity":
			return "Устройства подтверждают сохранённые криптографические ключи."
		case "tunnel_session":
			return "TLS-туннель готов принимать только разрешённые каналы."
		case "local_relays":
			return "Локальные Docker-маршруты доступны только этому приложению."
		case "docker_channel":
			return "Docker Engine отвечает через защищённый канал."
		case "sync_channel":
			return "Зарегистрированные проекты готовы к синхронизации."
		case "managed_wsl":
			return "Управляемая WSL-среда доступна на Windows."
		}
	}
	switch reason {
	case "host_unreachable":
		return "Windows-компьютер не отвечает в локальной сети."
	case "lan_blocked":
		return "Системная политика или сетевой фильтр блокирует локальную сеть."
	case "identity_mismatch":
		return "Сохранённая идентичность устройства не совпала. Повторите безопасное сопряжение."
	case "peer_busy":
		return "Windows уже обслуживает другой Mac-клиент."
	case "local_port_occupied":
		return "Нужный локальный порт занят другим процессом."
	case "wsl_unavailable":
		return "Управляемая WSL-среда сейчас недоступна."
	case "remote_connection_not_ready":
		return "Удалённое подключение ещё не готово."
	case "check_unavailable":
		return "Проверка недоступна в текущем состоянии."
	case "check_failed":
		return "Проверка не завершилась успешно."
	default:
		return "Проверка не завершилась успешно."
	}
}

func syncStatusLabel(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ready", "idle", "synced":
		return "Синхронизировано"
	case "starting", "scanning", "syncing":
		return "Синхронизация"
	case "paused", "stopped":
		return "Остановлено"
	case "error", "failed":
		return "Требует внимания"
	default:
		return "Ожидание синхронизации"
	}
}

func formatLastSuccess(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "Обновлялось ранее"
	}
	return parsed.Format("02.01.2006, 15:04")
}

func safeProjectError(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "":
		return ""
	case "remote_connection_not_ready", "not_connected":
		return "Удалённое подключение ещё не готово."
	case "workspace_unavailable", "folder_unavailable":
		return "Папка проекта сейчас недоступна."
	case "permission_denied":
		return "Remote Docker не может прочитать папку проекта."
	default:
		return "Синхронизация требует внимания. Откройте диагностику."
	}
}

func safeDiagnosticAction(action string) string {
	if strings.TrimSpace(action) == "" {
		return ""
	}
	return "Откройте диагностику и повторите проверку."
}

func operation(id string, enabled bool) Operation {
	label, pending, destructive := operationText(id)
	return Operation{ID: id, Label: label, PendingLabel: pending, Enabled: enabled, Destructive: destructive}
}

func operationText(id string) (string, string, bool) {
	switch id {
	case OperationEnableClient:
		return "Включить Remote Docker", "Запускаем…", false
	case OperationEnableHost:
		return "Начать ожидание подключения", "Запускаем…", false
	case OperationStartSearch:
		return "Найти компьютер", "Запускаем поиск…", false
	case OperationStopSearch:
		return "Остановить поиск", "Останавливаем поиск…", false
	case OperationConnect, OperationConnectTrusted:
		return "Подключиться", "Подключаемся…", false
	case OperationApprovePair:
		return "Код совпадает — разрешить", "Разрешаем…", false
	case OperationRejectPair:
		return "Отклонить", "Отклоняем…", true
	case OperationCancelPair:
		return "Отменить подключение", "Отменяем…", true
	case OperationPause:
		return "Поставить на паузу", "Останавливаем…", false
	case OperationDisconnect:
		return "Отключиться", "Отключаем…", true
	case OperationForgetDevice:
		return "Забыть", "Удаляем…", true
	case OperationAddProject:
		return "Добавить папку проекта", "Добавляем…", false
	case OperationRemoveProject:
		return "Удалить", "Удаляем…", true
	case OperationDiagnostics:
		return "Обновить проверки", "Проверяем…", false
	case OperationManualAddress:
		return "Ввести адрес вручную", "Проверяем адрес…", false
	case OperationQuit:
		return "Завершить работу", "Завершаем…", true
	default:
		return "Выполнить", "Выполняем…", false
	}
}

func applyPending(state *State, pendingID string, externallyBusy bool) {
	apply := func(operations []Operation) {
		for index := range operations {
			operations[index].Pending = pendingID != "" && operations[index].ID == pendingID
			if pendingID != "" || externallyBusy {
				operations[index].Enabled = false
			}
		}
	}
	apply(state.Operations)
	for index := range state.Devices {
		apply(state.Devices[index].Operations)
	}
	for index := range state.Projects {
		apply(state.Projects[index].Operations)
	}
}

func formatPairCode(code string) string {
	if len(code) == 6 {
		return code[:3] + " " + code[3:]
	}
	return code
}

func serviceStatus(name string, status localapi.ServiceStatus) string {
	switch status.State {
	case "ready":
		return name + " готов"
	case "starting":
		return name + " запускается"
	case "error":
		return name + " требует внимания"
	default:
		return name + " остановлен"
	}
}

func disconnectDetails(role string, disconnect localapi.DisconnectStatus) (string, string) {
	side := "Другое устройство"
	if disconnect.Initiator == "local" {
		side = "Это устройство"
	} else if role == "windows_host" {
		side = "Mac"
	} else {
		side = "Windows-компьютер"
	}
	switch disconnect.Reason {
	case "network_timeout":
		return "сеть", "Связь не восстановилась, удалённые процессы остановлены."
	case "peer_quit":
		return side, side + " завершил соединение."
	case "user_pause":
		return side, side + " поставил Remote Docker на паузу."
	default:
		return side, side + " отключил соединение."
	}
}

func diagnosticStatusLabel(status string) string {
	switch status {
	case DiagnosticReady:
		return "Готово"
	case DiagnosticRunning:
		return "Выполняется"
	case DiagnosticUnavailable:
		return "Недоступно"
	default:
		return "Нужна помощь"
	}
}

func formatBytes(value uint64) string {
	const (
		kilobyte = 1024
		megabyte = 1024 * kilobyte
		gigabyte = 1024 * megabyte
	)
	switch {
	case value >= gigabyte:
		return fmt.Sprintf("%.1f ГБ", float64(value)/gigabyte)
	case value >= megabyte:
		return fmt.Sprintf("%.0f МБ", float64(value)/megabyte)
	case value >= kilobyte:
		return fmt.Sprintf("%.0f КБ", float64(value)/kilobyte)
	default:
		return fmt.Sprintf("%d Б", value)
	}
}

func formatRate(value float64) string {
	if value >= 1024*1024 {
		return fmt.Sprintf("%.1f МБ/с", value/(1024*1024))
	}
	if value >= 1024 {
		return fmt.Sprintf("%.0f КБ/с", value/1024)
	}
	return fmt.Sprintf("%.0f Б/с", value)
}

func safeUnavailableReason(reasons ...string) string {
	for _, reason := range reasons {
		if strings.TrimSpace(reason) != "" {
			return reason
		}
	}
	return "Показатель пока недоступен"
}

func fallback(value, fallbackValue string) string {
	if strings.TrimSpace(value) == "" {
		return fallbackValue
	}
	return value
}
