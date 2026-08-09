package desktop

import (
	"errors"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/localapi"
)

func TestApplicationKeepsSafeActionErrorUntilNextAction(t *testing.T) {
	application := &Application{}
	application.setActionError(&localapi.RemoteError{
		Code:    localapi.ErrorNeedsAction,
		Message: "cannot pair /private/keys/remote-docker",
	})

	if got := application.actionError; got != "Действие сейчас недоступно. Проверьте состояние подключения." {
		t.Fatalf("action error = %q", got)
	}
	if got := safeActionMessage(errors.New("token at /private/keys/remote-docker")); got != "Не удалось выполнить действие. Попробуйте снова." {
		t.Fatalf("safeActionMessage() = %q", got)
	}

	application.clearActionError()
	if application.actionError != "" {
		t.Fatalf("next action did not clear error = %q", application.actionError)
	}
}
