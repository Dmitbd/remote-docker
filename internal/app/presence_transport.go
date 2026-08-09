package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/Dmitbd/remote-docker/internal/config"
)

const maxPresenceRPCMessage = 64 << 10

type presenceRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type presenceRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type presenceWireResult struct {
	SessionID   string `json:"session_id"`
	DockerReady bool   `json:"docker_ready"`
	SyncReady   bool   `json:"sync_ready"`
	Terminal    bool   `json:"terminal"`
	Reason      string `json:"reason"`
}

type presenceRPCProcess struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stop   func() error
}

type presenceRPCStarter func(context.Context) (*presenceRPCProcess, error)

// sshPresenceTransport owns one allow-listed SSH child for the complete
// presence lease. Calls are serialized so request IDs and responses cannot be
// confused with other Remote Docker RPC processes.
type sshPresenceTransport struct {
	starter presenceRPCStarter

	mu      sync.Mutex
	process *presenceRPCProcess
	reader  *bufio.Reader
	nextID  uint64
}

func newSSHPresenceTransport(starter presenceRPCStarter) *sshPresenceTransport {
	return &sshPresenceTransport{starter: starter}
}

func newProductionSSHPresenceTransport(store config.Store, sshConfigPath string) *sshPresenceTransport {
	return newSSHPresenceTransport(func(ctx context.Context) (*presenceRPCProcess, error) {
		cfg, err := loadAgentConfig(store)
		if err != nil || strings.TrimSpace(cfg.ActiveDevice) == "" {
			return nil, errors.New("trusted presence peer is unavailable")
		}
		alias := "remote-docker-device-" + cfg.ActiveDevice
		command := exec.CommandContext(ctx, "/usr/bin/ssh", presenceSSHArgs(sshConfigPath, alias)...)
		command.Env = os.Environ()
		command.Stderr = io.Discard
		stdin, err := command.StdinPipe()
		if err != nil {
			return nil, errors.New("open presence SSH input")
		}
		stdout, err := command.StdoutPipe()
		if err != nil {
			_ = stdin.Close()
			return nil, errors.New("open presence SSH output")
		}
		if err := command.Start(); err != nil {
			_ = stdin.Close()
			_ = stdout.Close()
			return nil, errors.New("start presence SSH process")
		}
		var stopOnce sync.Once
		stop := func() error {
			stopOnce.Do(func() {
				_ = stdin.Close()
				if command.Process != nil {
					_ = command.Process.Kill()
				}
				_ = command.Wait()
			})
			return nil
		}
		return &presenceRPCProcess{stdin: stdin, stdout: stdout, stop: stop}, nil
	})
}

func presenceSSHArgs(configPath, alias string) []string {
	return []string{
		"-F", configPath,
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "RequestTTY=no",
		alias, "remote-docker-remote", "rpc",
	}
}

func (t *sshPresenceTransport) Hello(ctx context.Context, hello PresenceHello) (PresenceHelloResult, error) {
	var result presenceWireResult
	if err := t.call(ctx, "presence.hello", map[string]any{
		"client_device_id": hello.ClientDeviceID,
		"client_name":      hello.ClientName,
		"app_version":      hello.AppVersion,
	}, &result); err != nil {
		return PresenceHelloResult{}, err
	}
	return PresenceHelloResult{SessionID: result.SessionID, DockerReady: result.DockerReady, SyncReady: result.SyncReady}, nil
}

func (t *sshPresenceTransport) Heartbeat(ctx context.Context, sessionID string, sequence uint64) (PresenceHeartbeatResult, error) {
	var result presenceWireResult
	if err := t.call(ctx, "presence.heartbeat", map[string]any{
		"session_id": sessionID, "monotonic_sequence": sequence,
	}, &result); err != nil {
		return PresenceHeartbeatResult{}, err
	}
	return PresenceHeartbeatResult{
		DockerReady: result.DockerReady, SyncReady: result.SyncReady,
		Terminal: result.Terminal, Reason: result.Reason,
	}, nil
}

func (t *sshPresenceTransport) Disconnect(ctx context.Context, sessionID, reason string) error {
	var result struct {
		Disconnected bool `json:"disconnected"`
	}
	err := t.call(ctx, "presence.disconnect", map[string]any{"session_id": sessionID, "reason": reason}, &result)
	closeErr := t.Close()
	if err == nil && !result.Disconnected {
		err = errors.New("presence disconnect was not acknowledged")
	}
	return errors.Join(err, closeErr)
}

func (t *sshPresenceTransport) call(ctx context.Context, method string, params any, result any) error {
	if t == nil || t.starter == nil {
		return ErrPresenceLease
	}
	if ctx == nil {
		ctx = context.Background()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.process == nil {
		process, err := t.starter(ctx)
		if err != nil || process == nil || process.stdin == nil || process.stdout == nil || process.stop == nil {
			return ErrPresenceLease
		}
		t.process = process
		t.reader = bufio.NewReaderSize(process.stdout, maxPresenceRPCMessage)
	}
	t.nextID++
	request := presenceRPCRequest{JSONRPC: "2.0", ID: t.nextID, Method: method, Params: params}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded)+1 > maxPresenceRPCMessage {
		t.closeLocked()
		return ErrPresenceLease
	}
	encoded = append(encoded, '\n')
	if _, err := t.process.stdin.Write(encoded); err != nil {
		t.closeLocked()
		return ErrPresenceLease
	}

	type readResult struct {
		line []byte
		err  error
	}
	read := make(chan readResult, 1)
	go func() {
		line, readErr := t.reader.ReadSlice('\n')
		read <- readResult{line: append([]byte(nil), line...), err: readErr}
	}()
	var received readResult
	select {
	case <-ctx.Done():
		t.closeLocked()
		return ctx.Err()
	case received = <-read:
	}
	if received.err != nil || len(received.line) > maxPresenceRPCMessage {
		t.closeLocked()
		return ErrPresenceLease
	}
	var response presenceRPCResponse
	if err := json.Unmarshal(received.line, &response); err != nil || response.JSONRPC != "2.0" || response.ID != request.ID || response.Error != nil || len(response.Result) == 0 {
		t.closeLocked()
		return ErrPresenceLease
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		t.closeLocked()
		return ErrPresenceLease
	}
	return nil
}

func (t *sshPresenceTransport) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closeLocked()
}

func (t *sshPresenceTransport) closeLocked() error {
	if t.process == nil {
		return nil
	}
	process := t.process
	t.process = nil
	t.reader = nil
	t.nextID = 0
	_ = process.stdout.Close()
	return process.stop()
}

var _ PresenceTransport = (*sshPresenceTransport)(nil)
