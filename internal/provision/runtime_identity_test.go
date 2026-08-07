package provision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/credentials"
)

func TestWSLRuntimeIdentityPreparerOwnsKeyInCredentialStoreAndUsesExactCommand(t *testing.T) {
	secrets := credentials.NewMemoryStore()
	runner := &recordingRuntimeIdentityRunner{statusErr: errors.New("not ready")}
	preparer := WSLRuntimeIdentityPreparer{
		Runner: runner, Secrets: secrets, WSLBinary: "wsl.exe", Distro: "remote-docker",
		Random: bytes.NewReader(bytes.Repeat([]byte{0x5a}, 32)),
	}

	if err := preparer.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	stored, err := secrets.Get(WindowsRuntimeCredentialOwner, WindowsRuntimeIdentityKeyCredential)
	if err != nil {
		t.Fatalf("read stored runtime key: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(stored))
	if err != nil || !bytes.Equal(decoded, bytes.Repeat([]byte{0x5a}, 32)) {
		t.Fatalf("stored key = %q error=%v", stored, err)
	}
	wantStatus := []string{"--distribution", "remote-docker", "--user", "root", "--exec", "/usr/local/bin/remote-docker-remote", "runtime-status"}
	wantState := []string{"--distribution", "remote-docker", "--user", "root", "--exec", "/usr/local/bin/remote-docker-remote", "runtime-identity-state"}
	wantPrepare := []string{"--distribution", "remote-docker", "--user", "root", "--exec", "/usr/local/bin/remote-docker-remote", "runtime-prepare"}
	if len(runner.commands) != 3 || !reflect.DeepEqual(runner.commands[0].Args, wantStatus) ||
		!reflect.DeepEqual(runner.commands[1].Args, wantState) || !reflect.DeepEqual(runner.commands[2].Args, wantPrepare) {
		t.Fatalf("commands = %#v", runner.commands)
	}
	var request RuntimeIdentityRequest
	if err := json.Unmarshal(runner.commands[2].Input, &request); err != nil {
		t.Fatalf("decode prepare request: %v", err)
	}
	if request.Key != string(stored) {
		t.Fatalf("request key differs from credential store")
	}
}

func TestWSLRuntimeIdentityPreparerNeverReplacesLostKeyForExistingEncryptedIdentity(t *testing.T) {
	secrets := credentials.NewMemoryStore()
	runner := &recordingRuntimeIdentityRunner{statusErr: errors.New("not ready"), identityExists: true}
	preparer := WSLRuntimeIdentityPreparer{
		Runner: runner, Secrets: secrets, Random: bytes.NewReader(bytes.Repeat([]byte{0x44}, 32)),
	}
	if err := preparer.Prepare(context.Background()); err == nil {
		t.Fatal("Prepare() regenerated a key for an existing encrypted identity")
	}
	if _, err := secrets.Get(WindowsRuntimeCredentialOwner, WindowsRuntimeIdentityKeyCredential); !errors.Is(err, credentials.ErrNotFound) {
		t.Fatalf("credential after refusal error = %v", err)
	}
	if runner.prepareCalls != 0 {
		t.Fatalf("prepare calls = %d, want 0", runner.prepareCalls)
	}
}

func TestWSLRuntimeIdentityPreparerDoesNotSendKeyWhenRuntimeIsReady(t *testing.T) {
	runner := &recordingRuntimeIdentityRunner{}
	preparer := WSLRuntimeIdentityPreparer{Runner: runner, Secrets: credentials.NewMemoryStore()}
	if err := preparer.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if len(runner.commands) != 1 || len(runner.commands[0].Input) != 0 {
		t.Fatalf("commands = %#v", runner.commands)
	}
}

func TestWSLRuntimeIdentityLifecycleRetriesAfterUnavailableWSL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &recordingRuntimeIdentityRunner{statusErr: errors.New("not ready"), prepareFailures: 1, onPrepare: func(call int) {
		if call == 2 {
			cancel()
		}
	}}
	preparer := WSLRuntimeIdentityPreparer{
		Runner: runner, Secrets: credentials.NewMemoryStore(),
		Random: bytes.NewReader(bytes.Repeat([]byte{0x21}, 32)),
	}
	err := preparer.Run(ctx, time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	if runner.prepareCalls != 2 {
		t.Fatalf("prepare calls = %d, want 2", runner.prepareCalls)
	}
}

type runtimeIdentityCommand struct {
	Binary string
	Args   []string
	Input  []byte
}

type recordingRuntimeIdentityRunner struct {
	commands        []runtimeIdentityCommand
	statusErr       error
	prepareFailures int
	prepareCalls    int
	onPrepare       func(int)
	identityExists  bool
}

func (r *recordingRuntimeIdentityRunner) Run(_ context.Context, command RuntimeIdentityCommand) error {
	var input []byte
	if command.Stdin != nil {
		input, _ = io.ReadAll(command.Stdin)
	}
	r.commands = append(r.commands, runtimeIdentityCommand{Binary: command.Binary, Args: append([]string(nil), command.Args...), Input: input})
	switch command.Args[len(command.Args)-1] {
	case "runtime-status":
		return r.statusErr
	case "runtime-identity-state":
		_, _ = io.WriteString(command.Stdout, `{"exists":`+strconv.FormatBool(r.identityExists)+`}`)
		return nil
	}
	r.prepareCalls++
	if r.onPrepare != nil {
		r.onPrepare(r.prepareCalls)
	}
	if r.prepareCalls <= r.prepareFailures {
		return errors.New("prepare failed")
	}
	return nil
}
