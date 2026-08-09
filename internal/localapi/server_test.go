package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestAgentLocalAPIUsesPerUserLocalTransport(t *testing.T) {
	directory, err := os.MkdirTemp("", "rd-api-")
	if err != nil {
		t.Fatalf("create local control test directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if runtime.GOOS != "windows" {
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatalf("protect local control test directory: %v", err)
		}
	}
	endpoint := filepath.Join(directory, "agent.sock")
	if runtime.GOOS == "windows" {
		endpoint = fmt.Sprintf(`\\.\pipe\remote-docker-agent-test-%d`, os.Getpid())
	}

	listener, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- (Server{Handler: HandlerFunc(func(_ context.Context, method Method, _ json.RawMessage) (any, error) {
			return map[string]string{"method": string(method)}, nil
		})}).Serve(ctx, listener)
	}()

	client := Client{Endpoint: endpoint}
	var result map[string]string
	if err := client.Call(ctx, MethodStatus, nil, &result); err != nil {
		cancel()
		<-serveDone
		t.Fatalf("Call() error = %v", err)
	}
	if result["method"] != string(MethodStatus) {
		t.Fatalf("result = %#v", result)
	}

	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
}

func TestAgentLocalAPIDispatchesEveryControlMethod(t *testing.T) {
	methods := []Method{
		MethodStatus,
		MethodEnable,
		MethodPause,
		MethodSearchStart,
		MethodSearchStop,
		MethodListDevices,
		MethodPairCandidates,
		MethodPairStart,
		MethodPairStatus,
		MethodPairApprove,
		MethodPairReject,
		MethodPairCancel,
		MethodDisconnect,
		MethodForgetDevice,
		MethodUnpair,
		MethodWorkspaceAdd,
		MethodWorkspaceList,
		MethodWorkspaceRemove,
		MethodSyncStatus,
		MethodPrepareDocker,
		MethodDoctor,
		MethodRecover,
		MethodShutdown,
		MethodResourceStatus,
	}

	for _, method := range methods {
		t.Run(string(method), func(t *testing.T) {
			serverConn, clientConn := net.Pipe()
			defer clientConn.Close()
			server := Server{
				Handler: HandlerFunc(func(_ context.Context, got Method, _ json.RawMessage) (any, error) {
					if got != method {
						t.Fatalf("method = %q, want %q", got, method)
					}
					return map[string]string{"method": string(got)}, nil
				}),
				AuthorizePeer: func(net.Conn) error { return nil },
			}
			serveDone := make(chan error, 1)
			go func() { serveDone <- server.ServeConn(context.Background(), serverConn) }()

			client := Client{Dial: pipeDialer(clientConn)}
			var result map[string]string
			if err := client.Call(context.Background(), method, map[string]string{"value": "input"}, &result); err != nil {
				t.Fatalf("Call() error = %v", err)
			}
			if result["method"] != string(method) {
				t.Fatalf("result = %#v", result)
			}
			if err := <-serveDone; err != nil {
				t.Fatalf("ServeConn() error = %v", err)
			}
		})
	}
}

func TestRecoverResultKeepsExistingFieldsAndAddsSafeAttempts(t *testing.T) {
	result := RecoverResult{
		State: "Ready", Message: "connected",
		Attempts: []RecoverAttempt{{Step: "reconnect", OK: true}},
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal RecoverResult: %v", err)
	}
	var legacy struct {
		State   string `json:"state"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(encoded, &legacy); err != nil {
		t.Fatalf("legacy decode RecoverResult: %v", err)
	}
	if legacy.State != "Ready" || legacy.Message != "connected" || !strings.Contains(string(encoded), `"attempts"`) {
		t.Fatalf("RecoverResult JSON = %s, legacy=%#v", encoded, legacy)
	}
}

func TestAgentLocalAPIRejectsTCPListener(t *testing.T) {
	listener := fakeListener{addr: fakeAddr{network: "tcp", address: "127.0.0.1:49152"}}
	err := (Server{}).Serve(context.Background(), listener)
	if !errors.Is(err, ErrInsecureTransport) {
		t.Fatalf("Serve() error = %v, want ErrInsecureTransport", err)
	}
}

func TestAgentLocalAPIRejectsDifferentPeerUser(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	server := Server{
		Handler: HandlerFunc(func(context.Context, Method, json.RawMessage) (any, error) {
			t.Fatal("handler called for rejected peer")
			return nil, nil
		}),
		AuthorizePeer: func(net.Conn) error { return ErrPeerOwnership },
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ServeConn(context.Background(), serverConn) }()

	var response wireResponse
	if err := json.NewDecoder(clientConn).Decode(&response); err != nil {
		t.Fatalf("decode peer rejection: %v", err)
	}
	if response.Error == nil || response.Error.Code != ErrorPeerForbidden {
		t.Fatalf("response error = %#v, want %q", response.Error, ErrorPeerForbidden)
	}
	if err := <-serveDone; err != nil {
		t.Fatalf("ServeConn() error = %v", err)
	}
}

func TestAgentLocalAPIRejectsSchemaMismatchBeforeDispatch(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	server := Server{
		Handler: HandlerFunc(func(context.Context, Method, json.RawMessage) (any, error) {
			t.Fatal("handler called for mismatched schema")
			return nil, nil
		}),
		AuthorizePeer: func(net.Conn) error { return nil },
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ServeConn(context.Background(), serverConn) }()

	client := Client{SchemaVersion: CurrentSchemaVersion + 1, Dial: pipeDialer(clientConn)}
	err := client.Call(context.Background(), MethodStatus, nil, nil)
	assertRemoteCode(t, err, ErrorSchemaMismatch)
	if err := <-serveDone; err != nil {
		t.Fatalf("ServeConn() error = %v", err)
	}
}

func TestDesktopLifecycleShipsInSchemaVersionFour(t *testing.T) {
	if CurrentSchemaVersion != 4 {
		t.Fatalf("CurrentSchemaVersion = %d, want 4 for desktop lifecycle contract", CurrentSchemaVersion)
	}
}

func TestAgentLocalAPIRedactsUnexpectedHandlerErrors(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	server := Server{
		Handler: HandlerFunc(func(context.Context, Method, json.RawMessage) (any, error) {
			return nil, errors.New("token=do-not-leak path=/private/account")
		}),
		AuthorizePeer: func(net.Conn) error { return nil },
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ServeConn(context.Background(), serverConn) }()

	client := Client{Dial: pipeDialer(clientConn)}
	err := client.Call(context.Background(), MethodDoctor, nil, nil)
	assertRemoteCode(t, err, ErrorInternal)
	if strings.Contains(err.Error(), "do-not-leak") || strings.Contains(err.Error(), "/private/account") {
		t.Fatalf("error was not redacted: %v", err)
	}
	if err := <-serveDone; err != nil {
		t.Fatalf("ServeConn() error = %v", err)
	}
}

func TestAgentLocalAPIExposesOnlyTypedSafeErrors(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	server := Server{
		Handler: HandlerFunc(func(context.Context, Method, json.RawMessage) (any, error) {
			return nil, &PublicError{Code: ErrorNeedsAction, Message: "pair a device first"}
		}),
		AuthorizePeer: func(net.Conn) error { return nil },
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ServeConn(context.Background(), serverConn) }()

	client := Client{Dial: pipeDialer(clientConn)}
	err := client.Call(context.Background(), MethodRecover, nil, nil)
	assertRemoteCode(t, err, ErrorNeedsAction)
	if !strings.Contains(err.Error(), "pair a device first") {
		t.Fatalf("error = %v, want public remediation", err)
	}
	if err := <-serveDone; err != nil {
		t.Fatalf("ServeConn() error = %v", err)
	}
}

func pipeDialer(conn net.Conn) DialFunc {
	var once sync.Once
	return func(context.Context) (net.Conn, error) {
		var result net.Conn
		once.Do(func() { result = conn })
		if result == nil {
			return nil, io.EOF
		}
		return result, nil
	}
}

func assertRemoteCode(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	var remote *RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("error = %T %v, want *RemoteError", err, err)
	}
	if remote.Code != code {
		t.Fatalf("remote code = %q, want %q", remote.Code, code)
	}
}

type fakeListener struct{ addr net.Addr }

func (fakeListener) Accept() (net.Conn, error) {
	return nil, errors.New("unexpected Accept call")
}

func (fakeListener) Close() error { return nil }

func (l fakeListener) Addr() net.Addr { return l.addr }

type fakeAddr struct {
	network string
	address string
}

func (a fakeAddr) Network() string { return a.network }

func (a fakeAddr) String() string { return a.address }
