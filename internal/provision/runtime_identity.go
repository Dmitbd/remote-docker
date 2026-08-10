package provision

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/Dmitbd/remote-docker/internal/credentials"
)

const (
	WindowsRuntimeCredentialOwner       = "windows-wsl-runtime"
	WindowsRuntimeIdentityKeyCredential = "runtime-identity-key"
)

type RuntimeIdentityRequest struct {
	Key string `json:"key"`
}

type RuntimeIdentityCommand struct {
	Binary string
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type RuntimeIdentityRunner interface {
	Run(context.Context, RuntimeIdentityCommand) error
}

type WSLRuntimeIdentityPreparer struct {
	Runner    RuntimeIdentityRunner
	Secrets   credentials.Store
	WSLBinary string
	Distro    string
	Random    io.Reader
}

func (p WSLRuntimeIdentityPreparer) Prepare(ctx context.Context) error {
	if p.Secrets == nil {
		return errors.New("Windows runtime credential store is unavailable")
	}
	if err := p.run(ctx, "runtime-status", nil, io.Discard); err == nil {
		return nil
	}
	encodedKey, err := p.runtimeKey(ctx)
	if err != nil {
		return err
	}
	defer clear(encodedKey)
	request, err := json.Marshal(RuntimeIdentityRequest{Key: string(encodedKey)})
	if err != nil {
		return errors.New("encode Windows runtime identity request")
	}
	defer clear(request)
	request = append(request, '\n')
	if err := p.run(ctx, "runtime-prepare", bytes.NewReader(request), io.Discard); err != nil {
		return errors.New("prepare managed WSL runtime identity")
	}
	return nil
}

func (p WSLRuntimeIdentityPreparer) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	for {
		_ = p.Prepare(ctx)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (p WSLRuntimeIdentityPreparer) runtimeKey(ctx context.Context) ([]byte, error) {
	stored, err := p.Secrets.Get(WindowsRuntimeCredentialOwner, WindowsRuntimeIdentityKeyCredential)
	if err == nil {
		decoded, decodeErr := base64.StdEncoding.DecodeString(string(stored))
		if decodeErr != nil || len(decoded) != 32 {
			return nil, errors.New("Windows runtime identity credential is invalid")
		}
		clear(decoded)
		return stored, nil
	}
	if !errors.Is(err, credentials.ErrNotFound) {
		return nil, fmt.Errorf("read Windows runtime identity credential: %w", err)
	}
	exists, err := p.encryptedIdentityExists(ctx)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, errors.New("Windows runtime identity credential is missing")
	}
	randomness := p.Random
	if randomness == nil {
		randomness = rand.Reader
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(randomness, key); err != nil {
		return nil, errors.New("generate Windows runtime identity credential")
	}
	encoded := []byte(base64.StdEncoding.EncodeToString(key))
	clear(key)
	if err := p.Secrets.Put(WindowsRuntimeCredentialOwner, WindowsRuntimeIdentityKeyCredential, encoded); err != nil {
		clear(encoded)
		return nil, fmt.Errorf("store Windows runtime identity credential: %w", err)
	}
	return encoded, nil
}

func (p WSLRuntimeIdentityPreparer) encryptedIdentityExists(ctx context.Context) (bool, error) {
	output := &boundedRuntimeOutput{remaining: 1024}
	if err := p.run(ctx, "runtime-identity-state", nil, output); err != nil {
		return false, errors.New("inspect managed WSL runtime identity")
	}
	var state struct {
		Exists bool `json:"exists"`
	}
	decoder := json.NewDecoder(&output.buffer)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return false, errors.New("inspect managed WSL runtime identity")
	}
	return state.Exists, nil
}

type boundedRuntimeOutput struct {
	buffer    bytes.Buffer
	remaining int
}

func (w *boundedRuntimeOutput) Write(value []byte) (int, error) {
	if len(value) > w.remaining {
		return 0, errors.New("managed WSL runtime identity output is too large")
	}
	w.remaining -= len(value)
	return w.buffer.Write(value)
}

func (p WSLRuntimeIdentityPreparer) run(ctx context.Context, operation string, stdin io.Reader, stdout io.Writer) error {
	runner := p.Runner
	if runner == nil {
		runner = execRuntimeIdentityRunner{}
	}
	binary := p.WSLBinary
	if binary == "" {
		binary = "wsl.exe"
	}
	distro := p.Distro
	if distro == "" {
		distro = defaultManagedDistro
	}
	return runner.Run(ctx, RuntimeIdentityCommand{
		Binary: binary,
		Args: []string{
			"--distribution", distro, "--user", "root", "--exec",
			"/usr/local/bin/remote-docker-remote", operation,
		},
		Stdin: stdin, Stdout: stdout, Stderr: io.Discard,
	})
}

type execRuntimeIdentityRunner struct{}

func (execRuntimeIdentityRunner) Run(ctx context.Context, command RuntimeIdentityCommand) error {
	process := exec.CommandContext(ctx, command.Binary, command.Args...)
	process.Stdin = command.Stdin
	process.Stdout = command.Stdout
	process.Stderr = command.Stderr
	configureHiddenProcess(process)
	return process.Run()
}

var _ interface {
	Run(context.Context, time.Duration) error
} = WSLRuntimeIdentityPreparer{}
