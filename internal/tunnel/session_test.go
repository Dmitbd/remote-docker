package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

func TestTunnelSessionCarriesFourConcurrentTypedStreams(t *testing.T) {
	clientConnection, serverConnection := net.Pipe()
	client, err := NewClientSession(clientConnection)
	if err != nil {
		t.Fatalf("NewClientSession() error = %v", err)
	}
	server, err := NewServerSession(serverConnection)
	if err != nil {
		t.Fatalf("NewServerSession() error = %v", err)
	}
	defer client.Close()
	defer server.Close()

	kinds := []StreamKind{StreamDockerSSH, StreamWorkspaceSync, StreamControl, StreamMetrics}
	var wait sync.WaitGroup
	for _, kind := range kinds {
		kind := kind
		wait.Add(1)
		go func() {
			defer wait.Done()
			stream, openErr := client.OpenStream(context.Background(), kind)
			if openErr != nil {
				t.Errorf("OpenStream(%s) error = %v", kind, openErr)
				return
			}
			defer stream.Close()
			payload := []byte(fmt.Sprintf("payload-%d", kind))
			if _, writeErr := stream.Write(payload); writeErr != nil {
				t.Errorf("Write(%s) error = %v", kind, writeErr)
			}
		}()
	}
	seen := make(map[StreamKind]string)
	for range kinds {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		kind, stream, acceptErr := server.AcceptStream(ctx)
		cancel()
		if acceptErr != nil {
			t.Fatalf("AcceptStream() error = %v", acceptErr)
		}
		payload, readErr := io.ReadAll(stream)
		_ = stream.Close()
		if readErr != nil {
			t.Fatalf("ReadAll(%s) error = %v", kind, readErr)
		}
		seen[kind] = string(payload)
	}
	wait.Wait()
	for _, kind := range kinds {
		if seen[kind] != fmt.Sprintf("payload-%d", kind) {
			t.Fatalf("stream %s payload = %q", kind, seen[kind])
		}
	}
}

func TestTunnelSessionSkipsStalledAndUnknownHeadersWithoutBlockingNextStream(t *testing.T) {
	clientConnection, serverConnection := net.Pipe()
	rawClient, _ := yamux.Client(clientConnection, yamuxConfig())
	server, err := NewServerSession(serverConnection)
	if err != nil {
		t.Fatalf("NewServerSession() error = %v", err)
	}
	defer rawClient.Close()
	defer server.Close()
	for _, header := range [][]byte{{'R', 'D'}, {'R', 'D', 'T', '1', 99}} {
		stream, openErr := rawClient.OpenStream()
		if openErr != nil {
			t.Fatalf("raw OpenStream() error = %v", openErr)
		}
		_, _ = stream.Write(header)
		valid, openErr := rawClient.OpenStream()
		if openErr != nil {
			t.Fatalf("valid raw OpenStream() error = %v", openErr)
		}
		if err := writeStreamHeader(valid, StreamControl); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		kind, accepted, acceptErr := server.AcceptStream(ctx)
		cancel()
		_ = stream.Close()
		if acceptErr != nil || accepted == nil || kind != StreamControl {
			t.Fatalf("AcceptStream after %x = %v, %v, %v", header, kind, accepted, acceptErr)
		}
		_ = accepted.Close()
	}
}

func TestTunnelSessionHeaderReadHonorsCancellationAndClearsDeadline(t *testing.T) {
	clientConnection, serverConnection := net.Pipe()
	rawClient, _ := yamux.Client(clientConnection, yamuxConfig())
	server, _ := NewServerSession(serverConnection)
	defer rawClient.Close()
	defer server.Close()

	stalled, _ := rawClient.OpenStream()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, err := server.AcceptStream(ctx)
		result <- err
	}()
	_, _ = stalled.Write([]byte{'R'})
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("AcceptStream cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled header read remained blocked")
	}
	_ = stalled.Close()

	valid, _ := rawClient.OpenStream()
	if err := writeStreamHeader(valid, StreamMetrics); err != nil {
		t.Fatal(err)
	}
	acceptCtx, acceptCancel := context.WithTimeout(context.Background(), time.Second)
	_, accepted, err := server.AcceptStream(acceptCtx)
	acceptCancel()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(streamHeaderReadTimeout + 50*time.Millisecond)
	go func() { _, _ = valid.Write([]byte("after-deadline")) }()
	payload := make([]byte, len("after-deadline"))
	if _, err := io.ReadFull(accepted, payload); err != nil || string(payload) != "after-deadline" {
		t.Fatalf("read after cleared header deadline = %q, %v", payload, err)
	}
	_ = accepted.Close()
}

func TestTunnelSessionCleansUpCancellationBeforeAccept(t *testing.T) {
	clientConnection, serverConnection := net.Pipe()
	rawClient, _ := yamux.Client(clientConnection, yamuxConfig())
	server, _ := NewServerSession(serverConnection)
	defer rawClient.Close()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := server.AcceptStream(ctx); err == nil {
		t.Fatal("AcceptStream accepted cancelled context")
	}
}
