package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestClientReconnectsAndStopsRetriesImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	dials := 0
	states := make(chan ClientState, 16)
	sessions := []*fakeSession{newFakeSession(), newFakeSession()}
	client := &Client{
		Dial: func(context.Context) (net.Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			dials++
			first, second := net.Pipe()
			_ = second.Close()
			return first, nil
		},
		NewSession: func(net.Conn) (Session, error) {
			mu.Lock()
			defer mu.Unlock()
			return sessions[dials-1], nil
		},
		OpenRelays: func(Session) ([]io.Closer, error) { return []io.Closer{nopCloser{}}, nil },
		OnState:    func(state ClientState, _ error) { states <- state },
		Wait:       func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil },
	}
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	waitClientState(t, states, ClientConnected)
	_ = sessions[0].Close()
	waitClientState(t, states, ClientConnected)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("client retries did not stop after cancellation")
	}
	mu.Lock()
	defer mu.Unlock()
	if dials != 2 {
		t.Fatalf("dial count = %d, want 2", dials)
	}
}

func TestReconnectDelayStaysWithinJitteredBounds(t *testing.T) {
	for attempt := 0; attempt < 10; attempt++ {
		delay := reconnectDelay(attempt)
		base := 250 * time.Millisecond
		for index := 0; index < attempt && base < 15*time.Second; index++ {
			base *= 2
			if base > 15*time.Second {
				base = 15 * time.Second
			}
		}
		if delay < time.Duration(float64(base)*0.8) || delay > time.Duration(float64(base)*1.2) {
			t.Fatalf("reconnectDelay(%d) = %s outside ±20%% of %s", attempt, delay, base)
		}
	}
}

func waitClientState(t *testing.T, states <-chan ClientState, want ClientState) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case state := <-states:
			if state == want {
				return
			}
		case <-deadline:
			t.Fatalf("did not observe state %s", want)
		}
	}
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
