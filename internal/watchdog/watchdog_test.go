package watchdog

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
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
