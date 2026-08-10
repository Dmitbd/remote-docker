package desktop

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"testing"
	"time"
)

func TestProcessLauncherStartsOneChildFocusesExistingAndStopsIt(t *testing.T) {
	var focusCalls atomic.Int32
	launcher := &ProcessLauncher{
		Executable: os.Args[0],
		Owner:      activeProcessOwner{},
		arguments:  []string{"-test.run=TestUIProcessHelper"},
		command: func(name string, args ...string) *exec.Cmd {
			command := exec.Command(name, args...)
			command.Env = append(os.Environ(), "REMOTE_DOCKER_UI_HELPER=1")
			return command
		},
		focus: func(context.Context) error {
			focusCalls.Add(1)
			return nil
		},
	}
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("first Show() error = %v", err)
	}
	if !launcher.Running() {
		t.Fatal("UI process is not running after Show")
	}
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("second Show() error = %v", err)
	}
	if focusCalls.Load() != 1 {
		t.Fatalf("focus calls = %d, want one without a duplicate child", focusCalls.Load())
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := launcher.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if launcher.Running() {
		t.Fatal("UI process is still running after Stop")
	}
}

func TestProcessLauncherRequiresOwnedAbsoluteExecutable(t *testing.T) {
	for _, launcher := range []*ProcessLauncher{
		{Executable: "", Owner: activeProcessOwner{}},
		{Executable: os.Args[0], Owner: inactiveProcessOwner{}},
	} {
		if err := launcher.Show(context.Background()); err == nil {
			t.Fatalf("Show() succeeded for invalid launcher %#v", launcher)
		}
	}
}

func TestUIProcessHelper(t *testing.T) {
	if os.Getenv("REMOTE_DOCKER_UI_HELPER") != "1" {
		return
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	<-interrupt
}

type activeProcessOwner struct{}

func (activeProcessOwner) Active() bool { return true }

type inactiveProcessOwner struct{}

func (inactiveProcessOwner) Active() bool { return false }
