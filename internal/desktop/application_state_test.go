package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
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

func TestApplicationRejectsOverlappingAction(t *testing.T) {
	application := &Application{}
	firstAction := application.beginAction()
	overlappingAction := application.beginAction()

	if overlappingAction != 0 {
		t.Fatalf("overlapping action sequence = %d, want rejected", overlappingAction)
	}
	if !application.completeAction(firstAction, nil) {
		t.Fatal("first action completion was ignored")
	}
	if application.actionBusy {
		t.Fatal("application remained busy after first action completed")
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

func TestReplaceUsesOneAtomicControllerOperation(t *testing.T) {
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
	calls := handler.WaitForCalls(t, 1)
	if got := calls[0]; got.Method != localapi.MethodReplaceDevice || got.Replace.OldDeviceID != "saved-windows" ||
		got.Replace.NewDevice != "new-windows" || got.Replace.LocalOnly {
		t.Fatalf("atomic replacement mutation = %#v", got)
	}
}

func TestReplaceIsOneSerializedOperationDespiteOverlappingAction(t *testing.T) {
	var snapshot lifecycle.Snapshot
	snapshot = lifecycle.Snapshot{
		State: lifecycle.StateSearching, TrustedPeers: 1, ConnectionLimit: 1,
		Peer: &lifecycle.Peer{ID: "saved-windows", Name: "Saved Windows"},
	}
	handler := &confirmationRecordingHandler{
		forgetStarted: make(chan struct{}),
		forgetRelease: make(chan struct{}),
	}
	handler.afterForget = func() {
		snapshot.TrustedPeers = 0
		snapshot.Peer = nil
	}
	application, confirm := newConfirmationApplication(handler, snapshotProvider(&snapshot))

	application.showReplaceConfirmation(DeviceRow{ID: "new-windows", Name: "New Windows"})
	(<-confirm)(true)
	waitForTestSignal(t, handler.forgetStarted, "replacement forget")
	application.perform(ActionStopSearch, "")
	if calls := handler.Calls(); len(calls) != 1 || calls[0].Method != localapi.MethodReplaceDevice {
		t.Fatalf("overlap reached controller during replacement: %#v", calls)
	}
	close(handler.forgetRelease)
	calls := handler.WaitForCalls(t, 1)
	if got := []localapi.Method{calls[0].Method}; !reflect.DeepEqual(got, []localapi.Method{localapi.MethodReplaceDevice}) {
		t.Fatalf("serialized replacement calls = %#v", got)
	}
}

func TestReplaceLocalOnlyFallbackRequiresSecondConfirmation(t *testing.T) {
	snapshot := lifecycle.Snapshot{
		State: lifecycle.StateSearching, TrustedPeers: 1, ConnectionLimit: 1,
		Peer: &lifecycle.Peer{ID: "saved-windows", Name: "Saved Windows"},
	}
	handler := &confirmationRecordingHandler{replaceError: &localapi.RemoteError{Code: localapi.ErrorUnavailable}}
	application, confirm := newConfirmationApplication(handler, snapshotProvider(&snapshot))

	application.showReplaceConfirmation(DeviceRow{ID: "new-windows", Name: "New Windows"})
	(<-confirm)(true)
	first := handler.WaitForCalls(t, 1)
	if first[0].Method != localapi.MethodReplaceDevice || first[0].Replace.LocalOnly {
		t.Fatalf("first replacement = %#v, want remote-first", first[0])
	}
	localConfirm := <-confirm
	if calls := handler.Calls(); len(calls) != 1 {
		t.Fatalf("local replacement started before second confirmation: %#v", calls)
	}
	handler.mu.Lock()
	handler.replaceError = nil
	handler.mu.Unlock()
	localConfirm(true)
	calls := handler.WaitForCalls(t, 2)
	if !calls[1].Replace.LocalOnly || calls[1].Replace.OldDeviceID != "saved-windows" || calls[1].Replace.NewDevice != "new-windows" {
		t.Fatalf("local-only replacement = %#v", calls[1])
	}
}

func TestReplaceConfirmationExplainsKeyRemovalAutomaticPairingAndCode(t *testing.T) {
	application := &Application{snapshot: snapshotProvider(&lifecycle.Snapshot{
		Peer: &lifecycle.Peer{ID: "saved-windows", Name: "Saved Windows"},
	})}
	var message string
	application.confirm = func(_, got, _ string, _ func(bool)) { message = got }

	application.showReplaceConfirmation(DeviceRow{ID: "new-windows", Name: "New Windows"})
	for _, phrase := range []string{"закрытый ключ", "автоматически", "шестизначного кода"} {
		if !strings.Contains(message, phrase) {
			t.Fatalf("replacement confirmation %q does not contain %q", message, phrase)
		}
	}
}

type confirmationCall struct {
	Method  localapi.Method
	Forget  localapi.ForgetDeviceParams
	Pair    localapi.PairStartParams
	Replace localapi.ReplaceDeviceParams
}

type confirmationRecordingHandler struct {
	mu            sync.Mutex
	calls         []confirmationCall
	updates       chan struct{}
	forgetError   error
	replaceError  error
	afterForget   func()
	forgetStarted chan struct{}
	forgetRelease chan struct{}
	forgetOnce    sync.Once
}

func (h *confirmationRecordingHandler) Handle(_ context.Context, method localapi.Method, raw json.RawMessage) (any, error) {
	call := confirmationCall{Method: method}
	switch method {
	case localapi.MethodForgetDevice:
		_ = json.Unmarshal(raw, &call.Forget)
	case localapi.MethodPairStart:
		_ = json.Unmarshal(raw, &call.Pair)
	case localapi.MethodReplaceDevice:
		_ = json.Unmarshal(raw, &call.Replace)
	}
	h.mu.Lock()
	h.calls = append(h.calls, call)
	forgetError := h.forgetError
	replaceError := h.replaceError
	afterForget := h.afterForget
	updates := h.updates
	h.mu.Unlock()
	if updates != nil {
		updates <- struct{}{}
	}
	if method == localapi.MethodForgetDevice && forgetError != nil {
		return nil, forgetError
	}
	if method == localapi.MethodReplaceDevice && replaceError != nil {
		return nil, replaceError
	}
	if (method == localapi.MethodForgetDevice || method == localapi.MethodReplaceDevice) && h.forgetStarted != nil {
		h.forgetOnce.Do(func() { close(h.forgetStarted) })
		<-h.forgetRelease
	}
	if (method == localapi.MethodForgetDevice || method == localapi.MethodReplaceDevice) && afterForget != nil {
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
		confirm: func(_, _, _ string, response func(bool)) {
			confirm <- response
		},
	}
	return application, confirm
}

func snapshotProvider(snapshot *lifecycle.Snapshot) SnapshotProvider {
	return func() lifecycle.Snapshot { return snapshot.Clone() }
}

func waitForTestSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for %s", name)
	}
}
