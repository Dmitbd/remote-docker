package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPresenceSSHArgsUsePinnedConfigAndDedicatedAllowedCommand(t *testing.T) {
	want := []string{
		"-F", "/managed/ssh_config",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "RequestTTY=no",
		"remote-docker-device-pc-1-control", "remote-docker-remote", "rpc",
	}
	if got := presenceSSHArgs("/managed/ssh_config", "remote-docker-device-pc-1-control"); !reflect.DeepEqual(got, want) {
		t.Fatalf("presence SSH args = %#v, want %#v", got, want)
	}
}

func TestSSHPresenceTransportDoesNotBindProcessToSuccessfulRequestContext(t *testing.T) {
	lifetimeCtx, cancelLifetime := context.WithCancel(context.Background())
	defer cancelLifetime()
	requestCtx, cancelRequest := context.WithCancel(context.Background())

	serverInput, clientInput := io.Pipe()
	clientOutput, serverOutput := io.Pipe()
	go func() {
		defer serverOutput.Close()
		decoder := json.NewDecoder(serverInput)
		encoder := json.NewEncoder(serverOutput)
		for index := 0; index < 2; index++ {
			var request presenceRPCRequest
			if err := decoder.Decode(&request); err != nil {
				return
			}
			encoded, _ := json.Marshal(presenceWireResult{
				SessionID: "session-1", DockerReady: true, SyncReady: true,
			})
			_ = encoder.Encode(presenceRPCResponse{
				JSONRPC: "2.0", ID: request.ID, Result: encoded,
			})
		}
	}()

	startedWith := make(chan context.Context, 1)
	stops := 0
	transport := newSSHPresenceTransport(lifetimeCtx, func(ctx context.Context) (*presenceRPCProcess, error) {
		startedWith <- ctx
		return &presenceRPCProcess{
			stdin: clientInput, stdout: clientOutput,
			stop: func() error {
				stops++
				_ = clientInput.Close()
				return nil
			},
		}, nil
	})

	hello, err := transport.Hello(requestCtx, PresenceHello{
		ClientDeviceID: "mac", ClientName: "MacBook", AppVersion: "0.2.9",
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	processCtx := <-startedWith
	if err := processCtx.Err(); err != nil {
		t.Fatalf("process context after successful request = %v", err)
	}
	if _, err := transport.Heartbeat(context.Background(), hello.SessionID, 1); err != nil {
		t.Fatalf("Heartbeat() after request cancellation = %v", err)
	}
	cancelLifetime()
	select {
	case <-processCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("process context survived session cancellation")
	}
	if err := transport.Close(); err != nil || stops != 1 {
		t.Fatalf("Close() error=%v stops=%d, want nil/1", err, stops)
	}
}

func TestSSHPresenceTransportRequestTimeoutStillStopsProcess(t *testing.T) {
	serverInput, clientInput := io.Pipe()
	clientOutput, serverOutput := io.Pipe()
	requestRead := make(chan struct{})
	go func() {
		var request presenceRPCRequest
		_ = json.NewDecoder(serverInput).Decode(&request)
		close(requestRead)
	}()

	stops := 0
	transport := newSSHPresenceTransport(context.Background(), func(context.Context) (*presenceRPCProcess, error) {
		return &presenceRPCProcess{
			stdin: clientInput, stdout: clientOutput,
			stop: func() error {
				stops++
				_ = clientInput.Close()
				_ = clientOutput.Close()
				_ = serverOutput.Close()
				return nil
			},
		}, nil
	})
	requestCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := transport.Hello(requestCtx, PresenceHello{
		ClientDeviceID: "mac", ClientName: "MacBook", AppVersion: "0.2.9",
	})
	<-requestRead
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Hello() error = %v, want deadline exceeded", err)
	}
	if stops != 1 {
		t.Fatalf("process stops = %d, want 1", stops)
	}
}

func TestSSHPresenceTransportKeepsOneProcessForHelloHeartbeatsAndDisconnect(t *testing.T) {
	serverInput, clientInput := io.Pipe()
	clientOutput, serverOutput := io.Pipe()
	requests := make(chan presenceRPCRequest, 3)
	go func() {
		defer serverOutput.Close()
		decoder := json.NewDecoder(serverInput)
		encoder := json.NewEncoder(serverOutput)
		sessionID := "session-1"
		for index := 0; index < 3; index++ {
			var request presenceRPCRequest
			if err := decoder.Decode(&request); err != nil {
				return
			}
			requests <- request
			result := presenceWireResult{SessionID: sessionID, DockerReady: true, SyncReady: true}
			if request.Method == "presence.disconnect" {
				_ = encoder.Encode(presenceRPCResponse{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{"disconnected":true}`)})
				return
			}
			encoded, _ := json.Marshal(result)
			_ = encoder.Encode(presenceRPCResponse{JSONRPC: "2.0", ID: request.ID, Result: encoded})
		}
	}()

	starts, stops := 0, 0
	transport := newSSHPresenceTransport(context.Background(), func(context.Context) (*presenceRPCProcess, error) {
		starts++
		return &presenceRPCProcess{
			stdin: clientInput, stdout: clientOutput,
			stop: func() error { stops++; _ = clientInput.Close(); return nil },
		}, nil
	})
	hello, err := transport.Hello(context.Background(), PresenceHello{ClientDeviceID: "mac", ClientName: "MacBook", AppVersion: "0.2.5"})
	if err != nil || hello.SessionID != "session-1" || !hello.DockerReady || !hello.SyncReady {
		t.Fatalf("Hello() = %#v, %v", hello, err)
	}
	heartbeat, err := transport.Heartbeat(context.Background(), hello.SessionID, 1)
	if err != nil || !heartbeat.DockerReady || !heartbeat.SyncReady {
		t.Fatalf("Heartbeat() = %#v, %v", heartbeat, err)
	}
	if err := transport.Disconnect(context.Background(), hello.SessionID, "user_disconnect"); err != nil {
		t.Fatalf("Disconnect() error = %v", err)
	}
	if starts != 1 || stops != 1 {
		t.Fatalf("process starts=%d stops=%d, want 1/1", starts, stops)
	}
	close(requests)
	methods := []string{}
	for request := range requests {
		methods = append(methods, request.Method)
	}
	if want := []string{"presence.hello", "presence.heartbeat", "presence.disconnect"}; !reflect.DeepEqual(methods, want) {
		t.Fatalf("RPC methods = %#v, want %#v", methods, want)
	}
}

func TestSSHPresenceTransportRejectsMismatchedOrOversizedResponsesAndStopsProcess(t *testing.T) {
	for _, test := range []struct {
		name     string
		response string
	}{
		{name: "mismatched id", response: `{"jsonrpc":"2.0","id":99,"result":{"session_id":"session"}}` + "\n"},
		{name: "oversized", response: strings.Repeat("x", maxPresenceRPCMessage+1) + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, writer := io.Pipe()
			reader := bufio.NewReader(strings.NewReader(test.response))
			stops := 0
			transport := newSSHPresenceTransport(context.Background(), func(context.Context) (*presenceRPCProcess, error) {
				return &presenceRPCProcess{
					stdin: writer, stdout: io.NopCloser(reader),
					stop: func() error { stops++; _ = writer.Close(); return nil },
				}, nil
			})
			go io.Copy(io.Discard, input)
			if _, err := transport.Hello(context.Background(), PresenceHello{ClientDeviceID: "mac", ClientName: "MacBook", AppVersion: "0.2.5"}); err == nil {
				t.Fatal("Hello() error = nil")
			}
			if stops != 1 {
				t.Fatalf("process stops = %d, want 1", stops)
			}
		})
	}
}
