package desktop

import (
	"reflect"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
	"github.com/Dmitbd/remote-docker/internal/localapi"
)

func TestBuildViewModelStartsPausedWithRoleSpecificManualEnable(t *testing.T) {
	for _, tt := range []struct {
		role      lifecycle.Role
		want      ActionID
		roleLabel string
	}{
		{role: lifecycle.RoleMacClient, want: ActionEnableClient, roleLabel: "Mac · отправляет Docker-команды"},
		{role: lifecycle.RoleWindowsHost, want: ActionEnableHost, roleLabel: "Windows · запускает Docker"},
	} {
		model := BuildViewModel(lifecycle.Snapshot{Role: tt.role, State: lifecycle.StatePaused, LocalName: "Device", ConnectionLimit: 1}, SectionConnection, time.Now())
		if model.Status != "На паузе" || model.Role != tt.roleLabel || !hasViewAction(model.Actions, tt.want) ||
			hasViewAction(model.Actions, ActionStartSearch) || !hasViewAction(model.Actions, ActionQuit) {
			t.Fatalf("paused model = %#v", model)
		}
	}
}

func TestBuildViewModelSeparatesMacSearchFromWindowsWaiting(t *testing.T) {
	mac := BuildViewModel(lifecycle.Snapshot{Role: lifecycle.RoleMacClient, State: lifecycle.StateClientReady}, SectionConnection, time.Now())
	if !hasViewAction(mac.Actions, ActionStartSearch) || hasViewAction(mac.Actions, ActionEnableHost) {
		t.Fatalf("Mac ready actions = %#v", mac.Actions)
	}
	windows := BuildViewModel(lifecycle.Snapshot{Role: lifecycle.RoleWindowsHost, State: lifecycle.StateHostWaiting}, SectionConnection, time.Now())
	if windows.Status != "Ожидает подключения" || hasViewAction(windows.Actions, ActionStartSearch) || !hasViewAction(windows.Actions, ActionPause) {
		t.Fatalf("Windows waiting model = %#v", windows)
	}
}

func TestBuildViewModelShowsSamePairCodeAndRoleCorrectActions(t *testing.T) {
	pairing := &lifecycle.Pairing{
		SessionID: "session", Peer: lifecycle.Peer{ID: "peer", Name: "Рабочий компьютер"},
		Code: "123456", Status: lifecycle.PairingPending, ExpiresAt: time.Now().Add(time.Minute),
	}
	mac := BuildViewModel(lifecycle.Snapshot{Role: lifecycle.RoleMacClient, State: lifecycle.StatePairing, Pairing: pairing}, SectionConnection, time.Now())
	if mac.PairCode != "123 456" || !hasViewAction(mac.Actions, ActionCancelPair) || hasViewAction(mac.Actions, ActionApprovePair) {
		t.Fatalf("Mac pairing model = %#v", mac)
	}
	windows := BuildViewModel(lifecycle.Snapshot{Role: lifecycle.RoleWindowsHost, State: lifecycle.StatePairing, Pairing: pairing}, SectionConnection, time.Now())
	if windows.PairCode != "123 456" || !hasViewAction(windows.Actions, ActionApprovePair) || !hasViewAction(windows.Actions, ActionRejectPair) ||
		hasViewAction(windows.Actions, ActionCancelPair) {
		t.Fatalf("Windows pairing model = %#v", windows)
	}
}

func TestBuildViewModelKeepsProblemPairingVisibleAndCancelable(t *testing.T) {
	pairing := &lifecycle.Pairing{
		SessionID: "session", Peer: lifecycle.Peer{ID: "peer", Name: "Windows"},
		Code: "123456", Status: lifecycle.PairingPending, ExpiresAt: time.Now().Add(time.Minute),
	}
	model := BuildViewModel(lifecycle.Snapshot{
		Role: lifecycle.RoleMacClient, State: lifecycle.StatePairing, Pairing: pairing,
		Problem: &lifecycle.Problem{Code: "runtime_stopped", Message: "Фоновый процесс остановился"},
	}, SectionConnection, time.Now())
	if model.Status != "Подтверждение устройства" || model.PairCode != "123 456" ||
		model.Notice != "Фоновый процесс остановился" || !hasViewAction(model.Actions, ActionCancelPair) {
		t.Fatalf("problem pairing model = %#v", model)
	}
}

func TestBuildViewModelShowsReplacementCancellationPendingWithRetryOnly(t *testing.T) {
	pairing := &lifecycle.Pairing{
		SessionID: "replacement-session", Peer: lifecycle.Peer{ID: "new", Name: "New Windows"},
		Code: "123456", Status: lifecycle.PairingCancellationPending, ExpiresAt: time.Now().Add(time.Minute),
	}
	model := BuildViewModel(lifecycle.Snapshot{
		Role: lifecycle.RoleMacClient, State: lifecycle.StatePairingCancellationPending,
		Pairing: pairing, TrustedPeers: 1, Peer: &lifecycle.Peer{ID: "saved", Name: "Saved Windows"},
	}, SectionConnection, time.Now())
	quit := viewActionByID(t, model.Actions, ActionQuit)
	if model.Status != "Отмена нового подключения" || model.PeerName != "New Windows" ||
		!hasViewAction(model.Actions, ActionCancelPair) || hasViewAction(model.Actions, ActionPause) ||
		quit.Enabled {
		t.Fatalf("cancellation-pending model = %#v", model)
	}
}

func viewActionByID(t *testing.T, actions []Action, id ActionID) Action {
	t.Helper()
	for _, action := range actions {
		if action.ID == id {
			return action
		}
	}
	t.Fatalf("action %q not found in %#v", id, actions)
	return Action{}
}

func TestBuildViewModelShowsRolesConnectionLimitAndRecoveryCountdown(t *testing.T) {
	connected := BuildViewModel(lifecycle.Snapshot{
		Role: lifecycle.RoleMacClient, State: lifecycle.StateConnected, TrustedPeers: 1, ConnectionLimit: 1,
		Peer: &lifecycle.Peer{Name: "Windows PC"}, Latency: 12 * time.Millisecond,
		Docker: lifecycle.DockerStatus{State: lifecycle.ServiceReady}, Sync: lifecycle.SyncStatus{State: lifecycle.ServiceReady},
	}, SectionConnection, time.Now())
	if connected.Status != "Соединено" || connected.ConnectionCount != "1 из 1" || connected.Latency != "12 мс" ||
		connected.Docker != "Docker готов" || connected.Sync != "Синхронизация готова" || !hasViewAction(connected.Actions, ActionDisconnect) {
		t.Fatalf("connected model = %#v", connected)
	}
	now := time.Now()
	reconnecting := BuildViewModel(lifecycle.Snapshot{
		Role: lifecycle.RoleWindowsHost, State: lifecycle.StateReconnecting,
		Recovery: &lifecycle.Recovery{Deadline: now.Add(41*time.Second + 100*time.Millisecond)},
	}, SectionConnection, now)
	if reconnecting.Countdown != "42 сек" || reconnecting.Status != "Восстановление связи" {
		t.Fatalf("reconnecting model = %#v", reconnecting)
	}
}

func TestBuildViewModelExplainsWhoEndedTheConnection(t *testing.T) {
	model := BuildViewModel(lifecycle.Snapshot{
		Role: lifecycle.RoleWindowsHost, State: lifecycle.StateHostWaiting,
		LastDisconnect: &lifecycle.Disconnect{Initiator: lifecycle.InitiatorPeer, Reason: lifecycle.ReasonPeerQuit},
	}, SectionConnection, time.Now())
	if model.Notice != "Mac завершил соединение" {
		t.Fatalf("disconnect notice = %q", model.Notice)
	}
}

func TestResourceRoleLabelsExplainWhereDockerRuns(t *testing.T) {
	roles := ResourceRoleLabels()
	if roles.Mac != "Mac передаёт исходники и команды" || roles.Windows != "Windows выполняет Docker-нагрузку" {
		t.Fatalf("ResourceRoleLabels() = %#v", roles)
	}
}

func TestBuildDeviceRowsShowsActionsByDeviceKind(t *testing.T) {
	newDevice := localapi.PairingCandidate{ID: "new", Name: "New Windows", Available: true, Unverified: true}
	savedAvailable := localapi.PairingCandidate{ID: "saved", Name: "Saved Windows", Trusted: true, Available: true}
	savedUnavailable := localapi.PairingCandidate{ID: "offline", Name: "Offline Windows", Trusted: true}

	rows := BuildDeviceRows(lifecycle.Snapshot{State: lifecycle.StateClientReady}, []localapi.PairingCandidate{
		savedUnavailable, newDevice, savedAvailable,
	})
	if got := []string{rows[0].ID, rows[1].ID, rows[2].ID}; !reflect.DeepEqual(got, []string{"offline", "saved", "new"}) {
		t.Fatalf("row order = %#v", got)
	}
	if got := deviceRowByID(t, rows, "new"); got.Status != "Новое устройство" || got.Kind != "new" ||
		!reflect.DeepEqual(got.Actions, []Action{{ID: ActionConnect, Label: "Подключиться", Enabled: false}}) {
		t.Fatalf("new row = %#v", got)
	}
	searching := BuildDeviceRows(lifecycle.Snapshot{State: lifecycle.StateSearching}, []localapi.PairingCandidate{newDevice})
	if got := deviceRowByID(t, searching, "new"); !reflect.DeepEqual(got.Actions, []Action{enabledAction(ActionConnect, "Подключиться")}) {
		t.Fatalf("searching new row = %#v", got)
	}
	if got := deviceRowByID(t, rows, "saved"); got.Status != "Сохранено · доступно" || got.Kind != "saved" ||
		!reflect.DeepEqual(got.Actions, []Action{
			enabledAction(ActionConnectTrusted, "Подключиться"),
			{ID: ActionForgetDevice, Label: "Забыть", Enabled: true, Destructive: true},
		}) {
		t.Fatalf("saved available row = %#v", got)
	}
	if got := deviceRowByID(t, rows, "offline"); got.Status != "Сохранено · недоступно" || got.Kind != "saved" ||
		!reflect.DeepEqual(got.Actions, []Action{
			{ID: ActionConnectTrusted, Label: "Подключиться", Enabled: false},
			{ID: ActionForgetDevice, Label: "Забыть", Enabled: true, Destructive: true},
		}) {
		t.Fatalf("saved unavailable row = %#v", got)
	}

	connected := BuildDeviceRows(lifecycle.Snapshot{
		State: lifecycle.StateConnected, Peer: &lifecycle.Peer{ID: "saved", Name: "Saved Windows"},
	}, []localapi.PairingCandidate{savedAvailable})
	if got := deviceRowByID(t, connected, "saved"); got.Status != "Соединено" || got.Kind != "connected" ||
		!reflect.DeepEqual(got.Actions, []Action{enabledAction(ActionDisconnect, "Отключиться")}) {
		t.Fatalf("connected row = %#v", got)
	}
}

func TestBuildDeviceRowsShowsConnectedPeerWithoutCandidates(t *testing.T) {
	rows := BuildDeviceRows(lifecycle.Snapshot{
		State: lifecycle.StateConnected,
		Peer:  &lifecycle.Peer{ID: "saved", Name: "Saved Windows"},
	}, nil)
	if len(rows) != 1 {
		t.Fatalf("row count = %d, want 1: %#v", len(rows), rows)
	}
	if got := rows[0]; got.ID != "saved" || got.Name != "Saved Windows" || got.Status != "Соединено" || got.Kind != "connected" ||
		!reflect.DeepEqual(got.Actions, []Action{enabledAction(ActionDisconnect, "Отключиться")}) {
		t.Fatalf("connected row = %#v", got)
	}
}

func TestBuildDeviceRowsShowsActivePeerWithoutForgetDuringBusyStates(t *testing.T) {
	for _, tt := range []struct {
		state       lifecycle.State
		status      string
		wantActions []Action
	}{
		{state: lifecycle.StateConnecting, status: "Подключение"},
		{state: lifecycle.StateReconnecting, status: "Восстановление связи", wantActions: []Action{enabledAction(ActionDisconnect, "Отключиться")}},
		{state: lifecycle.StateStopping, status: "Завершение работы"},
	} {
		t.Run(string(tt.state), func(t *testing.T) {
			rows := BuildDeviceRows(lifecycle.Snapshot{
				State: tt.state, Peer: &lifecycle.Peer{ID: "saved", Name: "Saved Windows"},
			}, []localapi.PairingCandidate{{ID: "saved", Name: "Saved Windows", Trusted: true, Available: true}})
			if len(rows) != 1 {
				t.Fatalf("row count = %d, want one: %#v", len(rows), rows)
			}
			if got := rows[0]; got.Kind != "active" || got.Status != tt.status || !reflect.DeepEqual(got.Actions, tt.wantActions) ||
				hasViewAction(got.Actions, ActionForgetDevice) {
				t.Fatalf("busy active row = %#v, want status=%q actions=%#v", got, tt.status, tt.wantActions)
			}
		})
	}
}

func deviceRowByID(t *testing.T, rows []DeviceRow, id string) DeviceRow {
	t.Helper()
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("row %q not found in %#v", id, rows)
	return DeviceRow{}
}

func hasViewAction(actions []Action, id ActionID) bool {
	for _, action := range actions {
		if action.ID == id {
			return true
		}
	}
	return false
}
