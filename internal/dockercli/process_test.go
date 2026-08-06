package dockercli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunnerPreservesProcessContract(t *testing.T) {
	helper := buildEchoHelper(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err := (Runner{}).Run(context.Background(), Invocation{
		Binary: helper,
		Args:   []string{"compose", "ps"},
		Env:    append(os.Environ(), "REMOTE_DOCKER_TEST_ENV=present"),
		Stdin:  strings.NewReader("hello from stdin"),
		Stdout: &stdout,
		Stderr: &stderr,
	})

	if got := ExitCode(err); got != 23 {
		t.Fatalf("ExitCode() = %d, want 23; error = %v", got, err)
	}
	for _, want := range []string{
		`arg[0]="compose"`,
		`arg[1]="ps"`,
		"env=present",
		"stdin=hello from stdin",
	} {
		if got := stdout.String(); !strings.Contains(got, want) {
			t.Fatalf("stdout = %q, want substring %q", got, want)
		}
	}
	if got := stderr.String(); got != "helper stderr\n" {
		t.Fatalf("stderr = %q, want %q", got, "helper stderr\n")
	}
}

func TestRunnerForwardsCancellationAsInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not provide POSIX process-group signals")
	}

	helper := buildEchoHelper(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutReader.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- (Runner{}).Run(ctx, Invocation{
			Binary: helper,
			Args:   []string{"wait-for-interrupt"},
			Env:    os.Environ(),
			Stdout: stdoutWriter,
			Stderr: io.Discard,
		})
		stdoutWriter.Close()
	}()

	scanner := bufio.NewScanner(stdoutReader)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatalf("helper readiness output = %q, error = %v", scanner.Text(), scanner.Err())
	}
	cancel()
	if !scanner.Scan() || scanner.Text() != "interrupted" {
		t.Fatalf("helper interrupt output = %q, error = %v", scanner.Text(), scanner.Err())
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after cancellation")
	}
}

func TestExitCode(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Fatalf("ExitCode(nil) = %d, want 0", got)
	}
	if got := ExitCode(errors.New("failure")); got != 1 {
		t.Fatalf("ExitCode(generic error) = %d, want 1", got)
	}
}

func buildEchoHelper(t *testing.T) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "echohelper")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	command := exec.Command("go", "build", "-o", binary, "./testdata/echohelper")
	command.Dir = "."
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build echohelper: %v\n%s", err, output)
	}

	return binary
}
