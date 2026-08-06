package sshtransport

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestForwardBuildsStrictLocalAndReverseCommands(t *testing.T) {
	tests := []struct {
		name  string
		spec  ForwardSpec
		flag  string
		value string
	}{
		{name: "local", spec: ForwardSpec{Direction: ForwardLocal, LocalPort: 8080, RemotePort: 80}, flag: "-L", value: "127.0.0.1:8080:127.0.0.1:80"},
		{name: "reverse", spec: ForwardSpec{Direction: ForwardReverse, LocalPort: 3000, RemotePort: 43000}, flag: "-R", value: "127.0.0.1:43000:127.0.0.1:3000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &forwardRunner{}
			forwarder := Forwarder{Runner: runner, Binary: "ssh", Probe: func(context.Context, int) error { return nil }}
			managed, err := forwarder.Start(context.Background(), "/managed/config", "remote-docker-device-pc", tt.spec)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"-F", "/managed/config", "-N", "-o", "ExitOnForwardFailure=yes", tt.flag, tt.value, "remote-docker-device-pc"}
			if !reflect.DeepEqual(runner.command.Args, want) {
				t.Fatalf("args = %#v, want %#v", runner.command.Args, want)
			}
			runner.process.done <- nil
			<-managed.Done()
			if err := managed.Err(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestForwardReturnsTypedConflictWithoutStartingSSH(t *testing.T) {
	runner := &forwardRunner{}
	forwarder := Forwarder{
		Runner: runner,
		Probe: func(context.Context, int) error {
			return &PortConflictError{Port: 8080}
		},
	}
	_, err := forwarder.Start(context.Background(), "/managed/config", "remote-docker-device-pc", ForwardSpec{
		Direction: ForwardLocal, LocalPort: 8080, RemotePort: 80,
	})
	var conflict *PortConflictError
	if !errors.As(err, &conflict) || conflict.Port != 8080 {
		t.Fatalf("Start() error = %v", err)
	}
	if runner.calls != 0 {
		t.Fatal("SSH started despite occupied port")
	}
}

type forwardRunner struct {
	calls   int
	command Command
	process *forwardTestProcess
}

func (r *forwardRunner) Start(_ context.Context, command Command) (Process, error) {
	r.calls++
	r.command = command
	r.process = &forwardTestProcess{done: make(chan error, 1)}
	return r.process, nil
}

func (r *forwardRunner) Run(context.Context, Command) error { return nil }

type forwardTestProcess struct {
	done chan error
}

func (p *forwardTestProcess) Kill() error { return nil }
func (p *forwardTestProcess) Wait() error { return <-p.done }
