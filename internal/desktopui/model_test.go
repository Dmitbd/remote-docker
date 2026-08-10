package desktopui

import (
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/localapi"
)

func TestBuildStateMapsEveryLifecycleStateForFixedRoles(t *testing.T) {
	tests := []struct {
		name       string
		role       string
		state      string
		wantRole   string
		wantAction string
	}{
		{"mac paused", "mac_client", "paused", "Mac · отправляет Docker-команды", OperationEnableClient},
		{"mac ready", "mac_client", "client_ready", "Mac · отправляет Docker-команды", OperationStartSearch},
		{"mac searching", "mac_client", "searching", "Mac · отправляет Docker-команды", OperationStopSearch},
		{"mac pairing", "mac_client", "pairing", "Mac · отправляет Docker-команды", OperationCancelPair},
		{"mac cancellation", "mac_client", "pairing_cancellation_pending", "Mac · отправляет Docker-команды", OperationCancelPair},
		{"mac connecting", "mac_client", "connecting", "Mac · отправляет Docker-команды", OperationQuit},
		{"mac connected", "mac_client", "connected", "Mac · отправляет Docker-команды", OperationDisconnect},
		{"mac reconnecting", "mac_client", "reconnecting", "Mac · отправляет Docker-команды", OperationDisconnect},
		{"mac stopping", "mac_client", "stopping", "Mac · отправляет Docker-команды", ""},
		{"mac attention", "mac_client", "needs_action", "Mac · отправляет Docker-команды", OperationDiagnostics},
		{"windows paused", "windows_host", "paused", "Windows · запускает Docker", OperationEnableHost},
		{"windows waiting", "windows_host", "host_waiting", "Windows · запускает Docker", OperationPause},
		{"windows pairing", "windows_host", "pairing", "Windows · запускает Docker", OperationApprovePair},
		{"windows connected", "windows_host", "connected", "Windows · запускает Docker", OperationDisconnect},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := BuildState(SnapshotInput{Status: localapi.StatusResult{
				Revision: 7, Role: test.role, State: test.state, LocalName: "Workstation",
				ConnectionLimit: 1,
			}}, "darwin", time.Unix(0, 0))
			if state.Revision != 7 || state.Role != test.wantRole || state.Lifecycle != test.state {
				t.Fatalf("state = %#v", state)
			}
			if test.wantAction == "" {
				if len(state.Operations) != 0 {
					t.Fatalf("terminal operations = %#v, want none", state.Operations)
				}
				return
			}
			if !hasOperation(state.Operations, test.wantAction) {
				t.Fatalf("operations = %#v, want %q", state.Operations, test.wantAction)
			}
			if test.state != "stopping" && !hasOperation(state.Operations, OperationQuit) {
				t.Fatalf("operations = %#v, want complete quit", state.Operations)
			}
		})
	}
}

func TestBuildStateKeepsDeviceActionsRoleAndConnectionSafe(t *testing.T) {
	input := SnapshotInput{
		Status: localapi.StatusResult{
			Revision: 9, Role: "mac_client", State: "searching", ConnectionLimit: 1,
			Peer: &localapi.LifecyclePeer{ID: "saved", Name: "Saved Windows"},
		},
		Candidates: []localapi.PairingCandidate{
			{ID: "saved", Name: "Saved Windows", Trusted: true, Available: true},
			{ID: "new", Name: "New Windows", Available: true, Unverified: true},
		},
	}
	state := BuildState(input, "darwin", time.Unix(0, 0))
	if len(state.Devices) != 2 || state.Devices[0].ID != "saved" || state.Devices[0].Role != "Windows-хост" {
		t.Fatalf("devices = %#v", state.Devices)
	}
	if !hasOperation(state.Devices[0].Operations, OperationConnectTrusted) ||
		!hasOperation(state.Devices[0].Operations, OperationForgetDevice) ||
		!hasOperation(state.Devices[1].Operations, OperationConnect) {
		t.Fatalf("device operations = %#v", state.Devices)
	}

	input.Status.State = "connected"
	state = BuildState(input, "darwin", time.Unix(0, 0))
	if hasEnabledOperation(state.Devices[1].Operations, OperationConnect) ||
		hasEnabledOperation(state.Devices[0].Operations, OperationConnectTrusted) {
		t.Fatalf("connected state permits a second connection: %#v", state.Devices)
	}
}

func TestBuildStateFormatsUnavailableResourcesAndSafeDiagnostics(t *testing.T) {
	state := BuildState(SnapshotInput{
		Status: localapi.StatusResult{Role: "mac_client", State: "connected"},
		Diagnostics: []localapi.DoctorCheck{
			{Name: "tls_identity", OK: true},
			{Name: "docker_channel", Status: "running", Message: "Проверяем Docker"},
			{Name: "sync_channel", Status: "unavailable", Message: "Соединение не активно"},
		},
	}, "darwin", time.Unix(0, 0))
	for _, card := range state.Resources.Cards {
		if card.Value == "0" || card.Value == "0%" || card.Value == "0 Б" {
			t.Fatalf("unavailable resource rendered as zero: %#v", card)
		}
	}
	if len(state.Diagnostics) != 3 || state.Diagnostics[0].Label != "Доверие между устройствами" ||
		state.Diagnostics[0].Status != DiagnosticReady || state.Diagnostics[1].Status != DiagnosticRunning ||
		state.Diagnostics[2].Status != DiagnosticUnavailable {
		t.Fatalf("diagnostics = %#v", state.Diagnostics)
	}
}

func hasOperation(operations []Operation, id string) bool {
	for _, operation := range operations {
		if operation.ID == id {
			return true
		}
	}
	return false
}

func hasEnabledOperation(operations []Operation, id string) bool {
	for _, operation := range operations {
		if operation.ID == id && operation.Enabled {
			return true
		}
	}
	return false
}
