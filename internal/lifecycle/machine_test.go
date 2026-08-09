package lifecycle

import (
	"errors"
	"testing"
	"time"
)

func TestMachineStartsPausedWithoutStartingWork(t *testing.T) {
	machine, err := NewMachine(RoleMacClient, "MacBook", WithClock(fixedClock(time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}

	got := machine.Snapshot()
	if got.State != StatePaused || got.Role != RoleMacClient || got.LocalName != "MacBook" {
		t.Fatalf("Snapshot() = %#v", got)
	}
	if got.Revision != 0 || got.TrustedPeers != 0 || got.ConnectionLimit != 1 || got.Terminal {
		t.Fatalf("initial lifecycle metadata = %#v", got)
	}
}

func TestMachineUsesRoleSpecificEnableStates(t *testing.T) {
	tests := []struct {
		name string
		role Role
		want State
	}{
		{name: "Mac client", role: RoleMacClient, want: StateClientReady},
		{name: "Windows host", role: RoleWindowsHost, want: StateHostWaiting},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			machine, err := NewMachine(tt.role, tt.name)
			if err != nil {
				t.Fatalf("NewMachine() error = %v", err)
			}
			got, err := machine.Apply(Event{Type: EventEnabled})
			if err != nil {
				t.Fatalf("Apply(EventEnabled) error = %v", err)
			}
			if got.State != tt.want || got.Revision != 1 {
				t.Fatalf("enabled snapshot = %#v, want state %q revision 1", got, tt.want)
			}
		})
	}
}

func TestMacSearchRequiresExplicitStartAndStop(t *testing.T) {
	machine := mustMachine(t, RoleMacClient)
	mustApply(t, machine, Event{Type: EventEnabled})

	if !machine.Allowed(CommandStartSearch) {
		t.Fatal("StartSearch must be allowed after explicitly enabling the Mac client")
	}
	searching := mustApply(t, machine, Event{Type: EventSearchStarted})
	if searching.State != StateSearching || !machine.Allowed(CommandStopSearch) {
		t.Fatalf("searching snapshot = %#v", searching)
	}
	ready := mustApply(t, machine, Event{Type: EventSearchStopped})
	if ready.State != StateClientReady {
		t.Fatalf("stopped search state = %q, want %q", ready.State, StateClientReady)
	}
}

func TestWindowsCannotStartMacSearch(t *testing.T) {
	machine := mustMachine(t, RoleWindowsHost)
	mustApply(t, machine, Event{Type: EventEnabled})

	_, err := machine.Apply(Event{Type: EventSearchStarted})
	var transition *TransitionError
	if !errors.As(err, &transition) {
		t.Fatalf("Apply(EventSearchStarted) error = %v, want TransitionError", err)
	}
	if machine.Snapshot().Revision != 1 {
		t.Fatalf("rejected event changed revision: %#v", machine.Snapshot())
	}
}

func TestPairingAndConnectionEnforceOneTrustedPeer(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	machine, err := NewMachine(RoleMacClient, "MacBook", WithClock(fixedClock(now)))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	mustApply(t, machine, Event{Type: EventEnabled})
	mustApply(t, machine, Event{Type: EventSearchStarted})
	pairing := Pairing{
		SessionID: "session-1",
		Peer:      Peer{ID: "windows-1", Name: "Render PC", OS: "Windows", Version: "11", Address: "192.168.1.20"},
		Code:      "123456",
		Status:    PairingPending,
		ExpiresAt: now.Add(2 * time.Minute),
	}
	paired := mustApply(t, machine, Event{Type: EventPairingStarted, Pairing: &pairing})
	if paired.State != StatePairing || paired.Pairing == nil || paired.Pairing.Code != "123456" {
		t.Fatalf("pairing snapshot = %#v", paired)
	}
	mustApply(t, machine, Event{Type: EventPairingApproved})
	connecting := mustApply(t, machine, Event{Type: EventPairingCompleted, Peer: &pairing.Peer})
	if connecting.State != StateConnecting || connecting.TrustedPeers != 1 || connecting.ConnectionLimit != 1 {
		t.Fatalf("connecting snapshot = %#v", connecting)
	}
	connected := mustApply(t, machine, Event{Type: EventConnected, Latency: 12 * time.Millisecond})
	if connected.State != StateConnected || connected.Peer == nil || connected.Peer.ID != "windows-1" || connected.Latency != 12*time.Millisecond {
		t.Fatalf("connected snapshot = %#v", connected)
	}

	replacement := pairing
	replacement.SessionID = "session-2"
	replacement.Peer.ID = "windows-2"
	_, err = machine.Apply(Event{Type: EventPairingStarted, Pairing: &replacement})
	if !errors.As(err, new(*TransitionError)) {
		t.Fatalf("second pairing error = %v, want TransitionError", err)
	}
}

func TestExplicitDisconnectStopsBeforeReturningToRoleIdleState(t *testing.T) {
	tests := []struct {
		name string
		role Role
		want State
	}{
		{name: "Mac", role: RoleMacClient, want: StateClientReady},
		{name: "Windows", role: RoleWindowsHost, want: StateHostWaiting},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			machine := connectedMachine(t, tt.role)
			stopping := mustApply(t, machine, Event{
				Type: EventDisconnectRequested,
				Disconnect: &Disconnect{Initiator: InitiatorLocal, Reason: ReasonUserDisconnect},
			})
			if stopping.State != StateStopping || !stopping.ActionInProgress {
				t.Fatalf("disconnect request snapshot = %#v", stopping)
			}
			idle := mustApply(t, machine, Event{Type: EventStopCompleted})
			if idle.State != tt.want || idle.ActionInProgress || idle.LastDisconnect == nil {
				t.Fatalf("disconnect completion snapshot = %#v", idle)
			}
			if idle.TrustedPeers != 1 || idle.Peer == nil {
				t.Fatalf("disconnect removed trust: %#v", idle)
			}
		})
	}
}

func TestTrustedPeerCanReconnectWithoutRepeatingPairing(t *testing.T) {
	machine := connectedMachine(t, RoleMacClient)
	mustApply(t, machine, Event{Type: EventDisconnectRequested, Disconnect: &Disconnect{Initiator: InitiatorLocal, Reason: ReasonUserDisconnect}})
	mustApply(t, machine, Event{Type: EventStopCompleted})
	if !machine.Allowed(CommandConnect) {
		t.Fatal("Connect must be allowed for the one trusted peer while disconnected")
	}
	connecting := mustApply(t, machine, Event{Type: EventConnectionStarted})
	if connecting.State != StateConnecting || connecting.TrustedPeers != 1 || connecting.Peer == nil {
		t.Fatalf("reconnection snapshot = %#v", connecting)
	}
}

func TestNetworkRecoveryUsesSixtySecondDeadline(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	machine, err := NewMachine(RoleWindowsHost, "Render PC", WithClock(fixedClock(now)))
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	connectMachine(t, machine)

	lost := mustApply(t, machine, Event{Type: EventNetworkLost})
	if lost.State != StateReconnecting || lost.Recovery == nil || !lost.Recovery.Deadline.Equal(now.Add(60*time.Second)) {
		t.Fatalf("network loss snapshot = %#v", lost)
	}
	recovered := mustApply(t, machine, Event{Type: EventNetworkRestored, Latency: 18 * time.Millisecond})
	if recovered.State != StateConnected || recovered.Recovery != nil || recovered.Latency != 18*time.Millisecond {
		t.Fatalf("network restored snapshot = %#v", recovered)
	}

	mustApply(t, machine, Event{Type: EventNetworkLost})
	stopping := mustApply(t, machine, Event{Type: EventRecoveryExpired})
	if stopping.State != StateStopping || stopping.LastDisconnect == nil || stopping.LastDisconnect.Reason != ReasonNetworkTimeout {
		t.Fatalf("expired recovery snapshot = %#v", stopping)
	}
	idle := mustApply(t, machine, Event{Type: EventStopCompleted})
	if idle.State != StateHostWaiting {
		t.Fatalf("post-timeout state = %q, want %q", idle.State, StateHostWaiting)
	}
}

func TestPauseAndQuitHaveDifferentTerminalIntent(t *testing.T) {
	machine := mustMachine(t, RoleWindowsHost)
	mustApply(t, machine, Event{Type: EventEnabled})
	pause := mustApply(t, machine, Event{Type: EventPauseRequested})
	if pause.State != StateStopping || pause.Terminal {
		t.Fatalf("pause request snapshot = %#v", pause)
	}
	paused := mustApply(t, machine, Event{Type: EventStopCompleted})
	if paused.State != StatePaused || paused.Terminal {
		t.Fatalf("pause completion snapshot = %#v", paused)
	}

	mustApply(t, machine, Event{Type: EventEnabled})
	quit := mustApply(t, machine, Event{Type: EventQuitRequested})
	if quit.State != StateStopping || !quit.Terminal {
		t.Fatalf("quit request snapshot = %#v", quit)
	}
	terminated := mustApply(t, machine, Event{Type: EventStopCompleted})
	if terminated.State != StatePaused || !terminated.Terminal {
		t.Fatalf("quit completion snapshot = %#v", terminated)
	}
}

func TestForgetClearsTrustOnlyWhileDisconnected(t *testing.T) {
	machine := connectedMachine(t, RoleMacClient)
	_, err := machine.Apply(Event{Type: EventTrustForgotten})
	if !errors.As(err, new(*TransitionError)) {
		t.Fatalf("forget while connected error = %v, want TransitionError", err)
	}
	mustApply(t, machine, Event{Type: EventDisconnectRequested, Disconnect: &Disconnect{Initiator: InitiatorLocal, Reason: ReasonUserDisconnect}})
	mustApply(t, machine, Event{Type: EventStopCompleted})
	forgotten := mustApply(t, machine, Event{Type: EventTrustForgotten})
	if forgotten.TrustedPeers != 0 || forgotten.Peer != nil {
		t.Fatalf("forgotten snapshot = %#v", forgotten)
	}
}

func TestSnapshotAndSubscriptionReturnIndependentCopies(t *testing.T) {
	machine := mustMachine(t, RoleMacClient)
	updates, cancel := machine.Subscribe()
	defer cancel()
	initial := <-updates
	if initial.State != StatePaused {
		t.Fatalf("initial subscribed state = %q", initial.State)
	}

	mustApply(t, machine, Event{Type: EventEnabled})
	latest := <-updates
	if latest.State != StateClientReady {
		t.Fatalf("subscribed state = %q", latest.State)
	}

	peer := Peer{ID: "pc-1", Name: "Render PC"}
	machine.mu.Lock()
	machine.snapshot.Peer = &peer
	machine.mu.Unlock()
	copyOne := machine.Snapshot()
	copyOne.Peer.Name = "mutated"
	copyTwo := machine.Snapshot()
	if copyTwo.Peer == nil || copyTwo.Peer.Name != "Render PC" {
		t.Fatalf("snapshot was mutated through a returned copy: %#v", copyTwo)
	}
}

func mustMachine(t *testing.T, role Role) *Machine {
	t.Helper()
	machine, err := NewMachine(role, "Device")
	if err != nil {
		t.Fatalf("NewMachine() error = %v", err)
	}
	return machine
}

func connectedMachine(t *testing.T, role Role) *Machine {
	t.Helper()
	machine := mustMachine(t, role)
	connectMachine(t, machine)
	return machine
}

func connectMachine(t *testing.T, machine *Machine) {
	t.Helper()
	mustApply(t, machine, Event{Type: EventEnabled})
	pairing := Pairing{
		SessionID: "session-1", Peer: Peer{ID: "peer-1", Name: "Peer"}, Code: "123456",
		Status: PairingPending, ExpiresAt: time.Now().Add(time.Minute),
	}
	if machine.Snapshot().Role == RoleMacClient {
		mustApply(t, machine, Event{Type: EventSearchStarted})
	}
	mustApply(t, machine, Event{Type: EventPairingStarted, Pairing: &pairing})
	mustApply(t, machine, Event{Type: EventPairingApproved})
	mustApply(t, machine, Event{Type: EventPairingCompleted, Peer: &pairing.Peer})
	mustApply(t, machine, Event{Type: EventConnected, Latency: 10 * time.Millisecond})
}

func mustApply(t *testing.T, machine *Machine, event Event) Snapshot {
	t.Helper()
	snapshot, err := machine.Apply(event)
	if err != nil {
		t.Fatalf("Apply(%q) error = %v", event.Type, err)
	}
	return snapshot
}

func fixedClock(now time.Time) func() time.Time {
	return func() time.Time { return now }
}
