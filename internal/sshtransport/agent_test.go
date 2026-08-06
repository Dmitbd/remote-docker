package sshtransport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAgentLoadsKeyThroughStdinAndCleansCallerBuffer(t *testing.T) {
	runner := &fakeCommandRunner{process: &fakeProcess{}}
	privateKey := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nsecret\n-----END OPENSSH PRIVATE KEY-----\n")
	socketPath := filepath.Join(shortPrivateDir(t), "a.sock")
	agent := Agent{
		Runner:      runner,
		AgentBinary: "/usr/bin/ssh-agent",
		AddBinary:   "/usr/bin/ssh-add",
		Env: []string{
			"PATH=/usr/bin:/bin",
			"SSH_AUTH_SOCK=/tmp/system-agent.sock",
			"SSH_AGENT_PID=999",
		},
		WaitForSocket: func(context.Context, string) error { return nil },
	}

	managed, err := agent.Start(context.Background(), socketPath, privateKey)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if runner.start.Binary != "/usr/bin/ssh-agent" ||
		!reflect.DeepEqual(runner.start.Args, []string{"-D", "-a", socketPath}) {
		t.Fatalf("ssh-agent command = %#v", runner.start)
	}
	if runner.run.Binary != "/usr/bin/ssh-add" || !reflect.DeepEqual(runner.run.Args, []string{"-"}) {
		t.Fatalf("ssh-add command = %#v", runner.run)
	}
	if string(runner.stdin) != "-----BEGIN OPENSSH PRIVATE KEY-----\nsecret\n-----END OPENSSH PRIVATE KEY-----\n" {
		t.Fatalf("ssh-add stdin = %q", runner.stdin)
	}
	if !containsEnv(runner.run.Env, "SSH_AUTH_SOCK="+socketPath) {
		t.Fatalf("ssh-add environment lacks managed socket: %#v", runner.run.Env)
	}
	for _, forbidden := range []string{"SSH_AUTH_SOCK=/tmp/system-agent.sock", "SSH_AGENT_PID=999"} {
		if containsEnv(runner.run.Env, forbidden) {
			t.Fatalf("ssh-add inherited user agent variable %q", forbidden)
		}
	}
	if !allZero(privateKey) {
		t.Fatalf("private key caller buffer was not cleared: %q", privateKey)
	}

	if err := managed.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if runner.process.kills != 1 || runner.process.waits != 1 {
		t.Fatalf("managed process kills=%d waits=%d, want one each", runner.process.kills, runner.process.waits)
	}
}

func TestAgentStopsChildAndCleansKeyWhenSSHAddFails(t *testing.T) {
	runner := &fakeCommandRunner{
		process: &fakeProcess{},
		runErr:  errors.New("ssh-add failed"),
	}
	privateKey := []byte("private")
	socketPath := filepath.Join(shortPrivateDir(t), "b.sock")
	agent := Agent{
		Runner:        runner,
		WaitForSocket: func(context.Context, string) error { return nil },
	}

	managed, err := agent.Start(context.Background(), socketPath, privateKey)
	if err == nil || managed != nil {
		t.Fatalf("Start() = (%#v, %v), want failure", managed, err)
	}
	if !allZero(privateKey) {
		t.Fatalf("private key was not cleared after failure: %q", privateKey)
	}
	if runner.process.kills != 1 || runner.process.waits != 1 {
		t.Fatalf("failed child kills=%d waits=%d, want one each", runner.process.kills, runner.process.waits)
	}
}

type fakeCommandRunner struct {
	start    Command
	run      Command
	stdin    []byte
	process  *fakeProcess
	startErr error
	runErr   error
}

func (r *fakeCommandRunner) Start(_ context.Context, command Command) (Process, error) {
	r.start = command
	return r.process, r.startErr
}

func (r *fakeCommandRunner) Run(_ context.Context, command Command) error {
	r.run = command
	if command.Stdin != nil {
		content, err := io.ReadAll(command.Stdin)
		if err != nil {
			return err
		}
		r.stdin = content
	}
	return r.runErr
}

type fakeProcess struct {
	kills int
	waits int
}

func (p *fakeProcess) Kill() error {
	p.kills++
	return nil
}

func (p *fakeProcess) Wait() error {
	p.waits++
	return nil
}

func containsEnv(environment []string, want string) bool {
	for _, variable := range environment {
		if variable == want {
			return true
		}
	}
	return false
}

func allZero(value []byte) bool {
	return bytes.Equal(value, make([]byte, len(value)))
}

func shortPrivateDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/private/tmp", "rd-agent-test-")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

var _ CommandRunner = (*fakeCommandRunner)(nil)
var _ Process = (*fakeProcess)(nil)
