package desktop

import (
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
)

func TestBuildViewModelStartsPausedWithRoleSpecificManualEnable(t *testing.T) {
	for _, tt := range []struct {
		role lifecycle.Role
		want ActionID
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

func hasViewAction(actions []Action, id ActionID) bool {
	for _, action := range actions {
		if action.ID == id {
			return true
		}
	}
	return false
}
