package watchdog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestChildAcceptsAuthenticatedCleanShutdownWithoutCleanup(t *testing.T) {
	token := strings.Repeat("a", tokenHexLength)
	input := encodeMessages(t,
		protocolMessage{Kind: messageInit, Token: token},
		protocolMessage{Kind: messageClean, Token: token},
	)
	cleanups := 0
	if code := runChild(context.Background(), input, func(context.Context) error { cleanups++; return nil }); code != 0 {
		t.Fatalf("runChild() code = %d", code)
	}
	if cleanups != 0 {
		t.Fatalf("clean shutdown cleanup calls = %d, want 0", cleanups)
	}
}

func TestChildCleansOwnedResourcesWhenParentPipeCloses(t *testing.T) {
	token := strings.Repeat("b", tokenHexLength)
	input := encodeMessages(t, protocolMessage{Kind: messageInit, Token: token})
	cleanups := 0
	if code := runChild(context.Background(), input, func(context.Context) error { cleanups++; return nil }); code != 0 {
		t.Fatalf("runChild() code = %d", code)
	}
	if cleanups != 1 {
		t.Fatalf("crash cleanup calls = %d, want 1", cleanups)
	}
}

func TestChildTreatsWrongCleanTokenAsCrash(t *testing.T) {
	token := strings.Repeat("c", tokenHexLength)
	input := encodeMessages(t,
		protocolMessage{Kind: messageInit, Token: token},
		protocolMessage{Kind: messageClean, Token: strings.Repeat("d", tokenHexLength)},
	)
	cleanups := 0
	if code := runChild(context.Background(), input, func(context.Context) error { cleanups++; return nil }); code == 0 {
		t.Fatal("runChild() accepted a clean message with the wrong token")
	}
	if cleanups != 1 {
		t.Fatalf("invalid-control cleanup calls = %d, want 1", cleanups)
	}
}

func TestChildRejectsUnauthenticatedDirectLaunchWithoutCleanup(t *testing.T) {
	cleanups := 0
	input := strings.NewReader(`{"kind":"init","token":"short"}` + "\n")
	if code := runChild(context.Background(), input, func(context.Context) error { cleanups++; return nil }); code == 0 {
		t.Fatal("runChild() accepted an invalid initialization token")
	}
	if cleanups != 0 {
		t.Fatalf("unauthenticated launch cleanup calls = %d, want 0", cleanups)
	}
}

func TestControllerCleanStopRetriesJoinWithoutRepeatingCleanSignal(t *testing.T) {
	input := &recordingWriteCloser{}
	done := make(chan error, 1)
	controller := &Controller{token: strings.Repeat("e", tokenHexLength), input: input, done: done}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	cancelFirst()

	if err := controller.CleanStop(firstCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("first CleanStop() error = %v, want context cancellation", err)
	}
	done <- nil
	close(done)
	if err := controller.CleanStop(context.Background()); err != nil {
		t.Fatalf("second CleanStop() error = %v", err)
	}
	if input.closeCalls != 1 {
		t.Fatalf("control pipe close calls = %d, want 1", input.closeCalls)
	}
	decoder := json.NewDecoder(bytes.NewReader(input.Bytes()))
	var message protocolMessage
	if err := decoder.Decode(&message); err != nil || message.Kind != messageClean || message.Token != controller.token {
		t.Fatalf("clean message = %#v error=%v", message, err)
	}
	if err := decoder.Decode(&message); !errors.Is(err, io.EOF) {
		t.Fatalf("second clean message decode error = %v, want EOF", err)
	}
}

func TestControllerCleanStopPreservesTerminalProcessErrorAcrossCalls(t *testing.T) {
	input := &recordingWriteCloser{}
	done := make(chan error, 1)
	terminalErr := errors.New("watchdog exit status 7")
	done <- terminalErr
	close(done)
	controller := &Controller{token: strings.Repeat("f", tokenHexLength), input: input, done: done}

	for call := 1; call <= 2; call++ {
		err := controller.CleanStop(context.Background())
		if !errors.Is(err, terminalErr) {
			t.Fatalf("CleanStop() call %d error = %v, want terminal error", call, err)
		}
	}
	if input.closeCalls != 1 {
		t.Fatalf("control pipe close calls = %d, want 1", input.closeCalls)
	}
}

func TestControllerCleanStopConcurrentCallsShareOneSignalAndTerminalJoin(t *testing.T) {
	input := &recordingWriteCloser{}
	done := make(chan error, 1)
	controller := &Controller{token: strings.Repeat("1", tokenHexLength), input: input, done: done}
	const callers = 8
	start := make(chan struct{})
	results := make(chan error, callers)
	for index := 0; index < callers; index++ {
		go func() {
			<-start
			results <- controller.CleanStop(context.Background())
		}()
	}
	close(start)
	done <- nil
	close(done)
	for index := 0; index < callers; index++ {
		if err := <-results; err != nil {
			t.Fatalf("concurrent CleanStop() error = %v", err)
		}
	}
	if input.closeCalls != 1 {
		t.Fatalf("control pipe close calls = %d, want 1", input.closeCalls)
	}
	decoder := json.NewDecoder(bytes.NewReader(input.Bytes()))
	var message protocolMessage
	if err := decoder.Decode(&message); err != nil || message.Kind != messageClean {
		t.Fatalf("clean message = %#v error=%v", message, err)
	}
	if err := decoder.Decode(&message); !errors.Is(err, io.EOF) {
		t.Fatalf("second clean message decode error = %v, want EOF", err)
	}
}

type recordingWriteCloser struct {
	mu         sync.Mutex
	buffer     bytes.Buffer
	closeCalls int
}

func (w *recordingWriteCloser) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.Write(value)
}

func (w *recordingWriteCloser) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closeCalls++
	return nil
}

func (w *recordingWriteCloser) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buffer.Bytes()...)
}

func encodeMessages(t *testing.T, messages ...protocolMessage) *bytes.Reader {
	t.Helper()
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	for _, message := range messages {
		if err := encoder.Encode(message); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}
	return bytes.NewReader(output.Bytes())
}
