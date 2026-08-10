package tunnel

import (
	"context"
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

func TestTunnelSessionRejectsMalformedAndUnknownHeadersAndCleansUpCancellation(t *testing.T) {
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
		_ = stream.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, accepted, acceptErr := server.AcceptStream(ctx)
		cancel()
		if acceptErr == nil || accepted != nil {
			t.Fatalf("AcceptStream(%x) = %v, %v", header, accepted, acceptErr)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := server.AcceptStream(ctx); err == nil {
		t.Fatal("AcceptStream accepted cancelled context")
	}
}
