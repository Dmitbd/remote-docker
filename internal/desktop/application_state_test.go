package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
	"github.com/Dmitbd/remote-docker/internal/localapi"
)

func TestApplicationKeepsSafeActionErrorUntilNextAction(t *testing.T) {
	application := &Application{}
	failedAction := application.beginAction()
	if !application.completeAction(failedAction, &localapi.RemoteError{
		Code:    localapi.ErrorNeedsAction,
		Message: "cannot pair /private/keys/remote-docker",
	}) {
		t.Fatal("failed action completion was ignored")
	}

	if got := application.actionError; got != "Действие сейчас недоступно. Проверьте состояние подключения." {
		t.Fatalf("action error = %q", got)
	}
	if got := safeActionMessage(errors.New("token at /private/keys/remote-docker")); got != "Не удалось выполнить действие. Попробуйте снова." {
		t.Fatalf("safeActionMessage() = %q", got)
	}

	application.beginAction()
	if application.actionError != "" {
		t.Fatalf("next action did not clear error = %q", application.actionError)
	}
}

func TestApplicationIgnoresSupersededActionCompletion(t *testing.T) {
	application := &Application{}
	slowAction := application.beginAction()
	newerAction := application.beginAction()

	if !application.completeAction(newerAction, nil) {
		t.Fatal("newer action completion was ignored")
	}
	if application.completeAction(slowAction, errors.New("token at /private/keys/remote-docker")) {
		t.Fatal("superseded action restored an error")
	}
	if application.actionError != "" {
		t.Fatalf("superseded completion changed error = %q", application.actionError)
	}
}

func TestForgetDialogCancelDoesNotCallController(t *testing.T) {
	handler := &confirmationRecordingHandler{}
	application, confirm := newConfirmationApplication(handler, snapshotProvider(&lifecycle.Snapshot{}))

	application.showForgetConfirmation(DeviceRow{ID: "saved-windows", Name: "Saved Windows"})
	(<-confirm)(false)

	if calls := handler.Calls(); len(calls) != 0 {
		t.Fatalf("mutation calls = %#v, want none", calls)
	}
}

func TestForgetUsesRemoteRevokeBeforeLocalCleanup(t *testing.T) {
	handler := &confirmationRecordingHandler{}
	application, confirm := newConfirmationApplication(handler, snapshotProvider(&lifecycle.Snapshot{}))

	application.showForgetConfirmation(DeviceRow{ID: "saved-windows", Name: "Saved Windows"})
	(<-confirm)(true)
	calls := handler.WaitForCalls(t, 1)
	if got := calls[0]; got.Method != localapi.MethodForgetDevice || got.Forget.DeviceID != "saved-windows" || got.Forget.LocalOnly {
		t.Fatalf("first mutation = %#v, want remote ForgetDevice for selected device", got)
	}
}

func TestUnavailableRemoteOffersExplicitLocalForget(t *testing.T) {
	handler := &confirmationRecordingHandler{forgetError: &localapi.RemoteError{Code: localapi.ErrorUnavailable}}
	application, confirm := newConfirmationApplication(handler, snapshotProvider(&lifecycle.Snapshot{}))

	application.showForgetConfirmation(DeviceRow{ID: "saved-windows", Name: "Saved Windows"})
	(<-confirm)(true)
	first := handler.WaitForCalls(t, 1)
	if first[0].Forget.LocalOnly {
		t.Fatalf("first mutation = %#v, want remote revoke", first[0])
	}
	localConfirm := <-confirm
	if calls := handler.Calls(); len(calls) != 1 {
		t.Fatalf("local cleanup started before explicit confirmation: %#v", calls)
	}
	handler.forgetError = nil
	localConfirm(true)
	calls := handler.WaitForCalls(t, 2)
	if got := calls[1]; got.Method != localapi.MethodForgetDevice || !got.Forget.LocalOnly {
		t.Fatalf("second mutation = %#v, want explicit local-only ForgetDevice", got)
	}
}

func TestReplaceWaitsForTrustRemovalBeforeStartingPair(t *testing.T) {
	var snapshot lifecycle.Snapshot
	snapshot = lifecycle.Snapshot{
		State: lifecycle.StateSearching, TrustedPeers: 1, ConnectionLimit: 1,
		Peer: &lifecycle.Peer{ID: "saved-windows", Name: "Saved Windows"},
	}
	handler := &confirmationRecordingHandler{}
	handler.afterForget = func() {
		snapshot.TrustedPeers = 0
		snapshot.Peer = nil
	}
	application, confirm := newConfirmationApplication(handler, snapshotProvider(&snapshot))

	application.showReplaceConfirmation(DeviceRow{ID: "new-windows", Name: "New Windows"})
	(<-confirm)(true)
	calls := handler.WaitForCalls(t, 2)
	if got := []localapi.Method{calls[0].Method, calls[1].Method}; got[0] != localapi.MethodForgetDevice || got[1] != localapi.MethodPairStart {
		t.Fatalf("mutation order = %#v, want ForgetDevice then PairStart", got)
	}
	if calls[0].Forget.LocalOnly || calls[1].Pair.Device != "new-windows" {
		t.Fatalf("replacement mutations = %#v", calls)
	}
}

type confirmationCall struct {
	Method localapi.Method
	Forget localapi.ForgetDeviceParams
	Pair   localapi.PairStartParams
}

type confirmationRecordingHandler struct {
	mu          sync.Mutex
	calls       []confirmationCall
	updates     chan struct{}
	forgetError error
	afterForget func()
}

func (h *confirmationRecordingHandler) Handle(_ context.Context, method localapi.Method, raw json.RawMessage) (any, error) {
	call := confirmationCall{Method: method}
	switch method {
	case localapi.MethodForgetDevice:
		_ = json.Unmarshal(raw, &call.Forget)
	case localapi.MethodPairStart:
		_ = json.Unmarshal(raw, &call.Pair)
	}
	h.mu.Lock()
	h.calls = append(h.calls, call)
	forgetError := h.forgetError
	afterForget := h.afterForget
	updates := h.updates
	h.mu.Unlock()
	if updates != nil {
		updates <- struct{}{}
	}
	if method == localapi.MethodForgetDevice && forgetError != nil {
		return nil, forgetError
	}
	if method == localapi.MethodForgetDevice && afterForget != nil {
		afterForget()
	}
	return nil, nil
}

func (h *confirmationRecordingHandler) Calls() []confirmationCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]confirmationCall(nil), h.calls...)
}

func (h *confirmationRecordingHandler) WaitForCalls(t *testing.T, want int) []confirmationCall {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		calls := h.Calls()
		if len(calls) >= want {
			return calls
		}
		select {
		case <-h.updates:
		case <-deadline:
			t.Fatalf("mutation calls = %#v, want at least %d", calls, want)
		}
	}
}

func newConfirmationApplication(handler *confirmationRecordingHandler, snapshot SnapshotProvider) (*Application, <-chan func(bool)) {
	confirm := make(chan func(bool), 2)
	handler.mu.Lock()
	if handler.updates == nil {
		handler.updates = make(chan struct{}, 4)
	}
	handler.mu.Unlock()
	application := &Application{
		controller: NewController(handler, snapshot),
		snapshot:   snapshot,
		showConfirm: func(_, _, _ string, response func(bool)) {
			confirm <- response
		},
	}
	return application, confirm
}

func snapshotProvider(snapshot *lifecycle.Snapshot) SnapshotProvider {
	return func() lifecycle.Snapshot { return snapshot.Clone() }
}
