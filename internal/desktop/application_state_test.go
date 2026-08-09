package desktop

import (
	"errors"
	"testing"

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
