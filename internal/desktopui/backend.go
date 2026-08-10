package desktopui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Dmitbd/remote-docker/internal/localapi"
)

var (
	ErrOperationPending  = errors.New("desktop operation is already pending")
	ErrOperationConflict = errors.New("another desktop operation is pending")
)

type OperationGuard struct {
	mu      sync.Mutex
	pending string
}

func NewOperationGuard() *OperationGuard { return &OperationGuard{} }

func (g *OperationGuard) Begin(id string) (func(), error) {
	if g == nil || strings.TrimSpace(id) == "" {
		return nil, errors.New("desktop operation is required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending == id {
		return nil, ErrOperationPending
	}
	if g.pending != "" {
		return nil, ErrOperationConflict
	}
	g.pending = id
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.pending == id {
				g.pending = ""
			}
			g.mu.Unlock()
		})
	}, nil
}

func (g *OperationGuard) Pending() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pending
}

type Backend struct {
	Client     localapi.Client
	Platform   string
	operations *OperationGuard
	now        func() time.Time

	cacheMu           sync.Mutex
	diagnostics       []localapi.DoctorCheck
	diagnosticsLoaded time.Time
}

func NewBackend(client localapi.Client, platform string) *Backend {
	return &Backend{
		Client: client, Platform: platform, operations: NewOperationGuard(), now: time.Now,
	}
}

func (b *Backend) Snapshot(ctx context.Context) (State, error) {
	if b == nil {
		return State{}, errors.New("desktop UI backend is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var status localapi.StatusResult
	if err := b.Client.Call(ctx, localapi.MethodStatus, nil, &status); err != nil {
		return State{}, publicBackendError("прочитать состояние Remote Docker", err)
	}
	input := SnapshotInput{Status: status, PendingID: b.operations.Pending()}
	if status.Role == "mac_client" && (status.State == "client_ready" || status.State == "searching" || status.Peer != nil) {
		var candidates localapi.PairCandidatesResult
		if err := b.Client.Call(ctx, localapi.MethodPairCandidates, nil, &candidates); err == nil {
			input.Candidates = candidates.Candidates
		}
	}
	var workspaces localapi.WorkspaceListResult
	if err := b.Client.Call(ctx, localapi.MethodWorkspaceList, nil, &workspaces); err == nil {
		input.Workspaces = workspaces.Workspaces
	}
	var syncStatus localapi.SyncStatusResult
	if err := b.Client.Call(ctx, localapi.MethodSyncStatus, nil, &syncStatus); err == nil {
		input.Sync = syncStatus
	}
	var resources localapi.ResourceStatusResult
	if err := b.Client.Call(ctx, localapi.MethodResourceStatus, localapi.ResourceStatusParams{Active: status.State == "connected"}, &resources); err == nil {
		input.Resources = resources
	}
	input.Diagnostics = b.cachedDiagnostics(ctx)
	now := time.Now()
	if b.now != nil {
		now = b.now()
	}
	return BuildState(input, b.Platform, now), nil
}

func (b *Backend) Perform(ctx context.Context, id, value string) (State, error) {
	if b == nil {
		return State{}, errors.New("desktop UI backend is unavailable")
	}
	release, err := b.operations.Begin(id)
	if err != nil {
		return State{}, err
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	method, params, err := b.resolve(ctx, id, value)
	if err != nil {
		return State{}, err
	}
	var result any
	if method == localapi.MethodDoctor {
		var diagnostics localapi.DoctorResult
		result = &diagnostics
		if err := b.Client.Call(ctx, method, params, result); err != nil {
			return State{}, publicBackendError("обновить диагностику", err)
		}
		b.storeDiagnostics(diagnostics.Checks)
	} else if err := b.Client.Call(ctx, method, params, nil); err != nil {
		return State{}, publicBackendError(operationFailureLabel(id), err)
	}
	release()
	released = true
	if id == OperationQuit {
		return State{Platform: b.Platform, Lifecycle: "stopping", Role: "Remote Docker", Connection: Connection{
			Status: "Завершение работы", Tone: "yellow", Headline: "Безопасно останавливаем процессы",
			Detail: "Окно закроется после завершения очистки.",
		}}, nil
	}
	return b.Snapshot(ctx)
}

func (b *Backend) Quit(ctx context.Context) error {
	if b == nil {
		return errors.New("desktop UI backend is unavailable")
	}
	release, err := b.operations.Begin(OperationQuit)
	if err != nil {
		return err
	}
	defer release()
	if err := b.Client.Call(ctx, localapi.MethodShutdown, nil, nil); err != nil {
		return publicBackendError("завершить работу", err)
	}
	return nil
}

func (b *Backend) resolve(ctx context.Context, id, value string) (localapi.Method, any, error) {
	value = strings.TrimSpace(value)
	switch id {
	case OperationEnableClient, OperationEnableHost:
		return localapi.MethodEnable, nil, nil
	case OperationStartSearch:
		return localapi.MethodSearchStart, nil, nil
	case OperationStopSearch:
		return localapi.MethodSearchStop, nil, nil
	case OperationConnect:
		if value == "" {
			return "", nil, errors.New("выберите Windows-компьютер")
		}
		return localapi.MethodPairStart, localapi.PairStartParams{Device: value}, nil
	case OperationManualAddress:
		params := localapi.PairStartParams{Address: value}
		if _, err := params.Target(); err != nil {
			return "", nil, errors.New("укажите частный IP-адрес Windows")
		}
		return localapi.MethodPairStart, params, nil
	case OperationConnectTrusted:
		if value == "" {
			return "", nil, errors.New("выберите сохранённое устройство")
		}
		return localapi.MethodConnect, nil, nil
	case OperationApprovePair, OperationRejectPair, OperationCancelPair:
		var status localapi.StatusResult
		if err := b.Client.Call(ctx, localapi.MethodStatus, nil, &status); err != nil {
			return "", nil, publicBackendError("прочитать pairing", err)
		}
		if status.Pairing == nil || status.Pairing.SessionID == "" {
			return "", nil, errors.New("запрос на подключение уже завершён")
		}
		method := localapi.MethodPairApprove
		if id == OperationRejectPair {
			method = localapi.MethodPairReject
		} else if id == OperationCancelPair {
			method = localapi.MethodPairCancel
		}
		return method, localapi.PairSessionParams{SessionID: status.Pairing.SessionID}, nil
	case OperationPause:
		return localapi.MethodPause, nil, nil
	case OperationDisconnect:
		return localapi.MethodDisconnect, localapi.DisconnectParams{}, nil
	case OperationForgetDevice:
		if value == "" {
			return "", nil, errors.New("выберите сохранённое устройство")
		}
		return localapi.MethodForgetDevice, localapi.ForgetDeviceParams{DeviceID: value}, nil
	case OperationAddProject:
		if b.Platform != "darwin" {
			return "", nil, errors.New("папки проектов добавляются только на Mac")
		}
		canonical, err := canonicalWorkspacePath(value)
		if err != nil {
			return "", nil, err
		}
		return localapi.MethodWorkspaceAdd, localapi.WorkspaceAddParams{Path: canonical}, nil
	case OperationRemoveProject:
		if value == "" {
			return "", nil, errors.New("выберите проект")
		}
		return localapi.MethodWorkspaceRemove, localapi.WorkspaceRemoveParams{ID: value}, nil
	case OperationDiagnostics:
		return localapi.MethodDoctor, nil, nil
	case OperationQuit:
		return localapi.MethodShutdown, nil, nil
	default:
		return "", nil, errors.New("действие недоступно")
	}
}

func canonicalWorkspacePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("выберите папку проекта")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", errors.New("не удалось проверить папку проекта")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", errors.New("выбранная папка проекта недоступна")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", errors.New("выберите существующую папку проекта")
	}
	return filepath.Abs(canonical)
}

func (b *Backend) cachedDiagnostics(ctx context.Context) []localapi.DoctorCheck {
	b.cacheMu.Lock()
	loaded := b.diagnosticsLoaded
	cached := append([]localapi.DoctorCheck(nil), b.diagnostics...)
	b.cacheMu.Unlock()
	if len(cached) > 0 && time.Since(loaded) < 10*time.Second {
		return cached
	}
	var result localapi.DoctorResult
	diagnosticCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := b.Client.Call(diagnosticCtx, localapi.MethodDoctor, nil, &result); err != nil {
		return cached
	}
	b.storeDiagnostics(result.Checks)
	return append([]localapi.DoctorCheck(nil), result.Checks...)
}

func (b *Backend) storeDiagnostics(checks []localapi.DoctorCheck) {
	b.cacheMu.Lock()
	b.diagnostics = append([]localapi.DoctorCheck(nil), checks...)
	b.diagnosticsLoaded = time.Now()
	b.cacheMu.Unlock()
}

func operationFailureLabel(id string) string {
	switch id {
	case OperationConnect, OperationConnectTrusted, OperationManualAddress:
		return "подключиться к Windows"
	case OperationForgetDevice:
		return "забыть устройство"
	case OperationAddProject:
		return "добавить проект"
	case OperationRemoveProject:
		return "удалить проект"
	case OperationQuit:
		return "завершить работу"
	default:
		return "выполнить действие"
	}
}

func publicBackendError(action string, err error) error {
	var remote *localapi.RemoteError
	if errors.As(err, &remote) && strings.TrimSpace(remote.Message) != "" {
		return fmt.Errorf("%s: %s", action, remote.Message)
	}
	return fmt.Errorf("%s: операция недоступна", action)
}
