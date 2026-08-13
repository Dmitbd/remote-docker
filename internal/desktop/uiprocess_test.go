package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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

func TestProcessLauncherRestartsNaturallyExitedChildOnce(t *testing.T) {
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, func(start int32, command *exec.Cmd) {
		if start == 1 {
			command.Env = append(command.Env, "REMOTE_DOCKER_UI_HELPER_ACTION=exit")
		}
	})
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("first Show() error = %v", err)
	}
	waitForLauncherChild(t, launcher)
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("second Show() error = %v", err)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("starts = %d, want 2", got)
	}
	stopLauncherForTest(t, launcher)
}

func TestProcessLauncherRecoversExactOwnedChildOnceAfterFocusFailure(t *testing.T) {
	wantFocusErr := errors.New("focus timeout")
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, nil)
	launcher.focus = func(context.Context) error { return wantFocusErr }
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("first Show() error = %v", err)
	}
	launcher.mu.Lock()
	original := launcher.process.Process
	launcher.mu.Unlock()
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("recovery Show() error = %v", err)
	}
	launcher.mu.Lock()
	replacement := launcher.process.Process
	launcher.mu.Unlock()
	if original == replacement {
		t.Fatal("recovery retained the unresponsive exact child")
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("starts = %d, want one initial and one replacement", got)
	}
	stopLauncherForTest(t, launcher)
}

func TestProcessLauncherRecoveryWaitHonorsCallerTimeout(t *testing.T) {
	var starts atomic.Int32
	ready := make(chan struct{})
	readyOutput := &readyWriter{ready: ready}
	launcher := newUIProcessTestLauncher(&starts, func(_ int32, command *exec.Cmd) {
		command.Env = append(command.Env, "REMOTE_DOCKER_UI_HELPER_ACTION=ignore-interrupt")
		command.Stdout = readyOutput
	})
	launcher.focus = func(context.Context) error { return errors.New("focus failed") }
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("first Show() error = %v", err)
	}
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("helper did not become ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := launcher.Show(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("recovery Show() error = %v, want deadline exceeded", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("starts = %d after timeout, want no replacement", got)
	}
	launcher.mu.Lock()
	command := launcher.process
	launcher.mu.Unlock()
	if command != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
	waitForLauncherChild(t, launcher)
}

func TestProcessLauncherReservesCallerDeadlineForRecoveryAfterFocusTimeout(t *testing.T) {
	var starts atomic.Int32
	ready := make(chan struct{})
	launcher := newUIProcessTestLauncher(&starts, func(_ int32, command *exec.Cmd) {
		command.Env = append(command.Env, "REMOTE_DOCKER_UI_HELPER_ACTION=ready")
		command.Stdout = &readyWriter{ready: ready}
	})
	launcher.focus = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("first Show() error = %v", err)
	}
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("helper did not become ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := launcher.Show(ctx); err != nil {
		t.Fatalf("Show() after bounded focus timeout = %v, want successful replacement", err)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("starts = %d, want one replacement within caller deadline", got)
	}
	stopLauncherForTest(t, launcher)
}

func TestProcessLauncherConcurrentRecoveryStartsOneReplacement(t *testing.T) {
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, nil)
	focusEntered := make(chan struct{})
	releaseFocus := make(chan struct{})
	launcher.focus = func(context.Context) error {
		close(focusEntered)
		<-releaseFocus
		return errors.New("focus failed")
	}
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("first Show() error = %v", err)
	}
	leaderResult := make(chan error, 1)
	go func() { leaderResult <- launcher.Show(context.Background()) }()
	<-focusEntered
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	cancelWaiter()
	if err := launcher.Show(waiterCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("concurrent waiter error = %v, want canceled while sharing recovery", err)
	}
	close(releaseFocus)
	if err := <-leaderResult; err != nil {
		t.Fatalf("recovery leader error = %v", err)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("starts = %d, want exactly one replacement", got)
	}
	stopLauncherForTest(t, launcher)
}

func TestProcessLauncherRecoveryFailsClosedWhenStopChangesOwnership(t *testing.T) {
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, nil)
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("first Show() error = %v", err)
	}
	launcher.focus = func(context.Context) error {
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := launcher.Stop(stopCtx); err != nil {
			t.Fatalf("concurrent Stop() error = %v", err)
		}
		return errors.New("focus failed")
	}
	err := launcher.Show(context.Background())
	if err == nil || !errors.Is(err, errUIProcessLauncherClosed) {
		t.Fatalf("recovery Show() error = %v, want launcher closed", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("starts = %d, want no replacement after ownership change", got)
	}
}

func TestProcessLauncherFocusSuccessFailsClosedWhenChildStopsConcurrently(t *testing.T) {
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, nil)
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("first Show() error = %v", err)
	}
	launcher.focus = func(context.Context) error {
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return launcher.Stop(stopCtx)
	}
	err := launcher.Show(context.Background())
	if !errors.Is(err, errUIChildOwnershipChanged) {
		t.Fatalf("Show() error = %v, want ownership changed after stale focus success", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("starts = %d, want no replacement after concurrent Stop", got)
	}
}

func TestProcessLauncherFocusFailureAfterNaturalExitStartsOneReplacement(t *testing.T) {
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, nil)
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("first Show() error = %v", err)
	}
	launcher.focus = func(context.Context) error {
		launcher.mu.Lock()
		command := launcher.process
		done := launcher.done
		launcher.mu.Unlock()
		if err := command.Process.Kill(); err != nil {
			t.Fatalf("kill natural-exit fixture: %v", err)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("exit callback did not complete during focus")
		}
		return errors.New("focus failed after exit")
	}
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("Show() after natural exit during focus error = %v", err)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("starts = %d, want exactly one replacement", got)
	}
	stopLauncherForTest(t, launcher)
}

func TestProcessLauncherRecoveryUsesExactKillFallbackAfterSignalFailure(t *testing.T) {
	wantSignalErr := errors.New("signal unsupported")
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, nil)
	launcher.focus = func(context.Context) error { return errors.New("focus failed") }
	launcher.signal = func(*os.Process, os.Signal) error { return wantSignalErr }
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("first Show() error = %v", err)
	}
	err := launcher.Show(context.Background())
	if err != nil {
		t.Fatalf("recovery Show() error = %v, want successful exact kill fallback", err)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("starts = %d, want one replacement after fallback", got)
	}
	launcher.signal = nil
	stopLauncherForTest(t, launcher)
}

func TestProcessLauncherRecoveryPropagatesSignalAndKillFailureWithoutRetry(t *testing.T) {
	wantSignalErr := errors.New("signal failed")
	wantKillErr := errors.New("kill failed")
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, nil)
	launcher.focus = func(context.Context) error { return errors.New("focus failed") }
	launcher.signal = func(*os.Process, os.Signal) error { return wantSignalErr }
	launcher.kill = func(*os.Process) error { return wantKillErr }
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("first Show() error = %v", err)
	}
	err := launcher.Show(context.Background())
	if !errors.Is(err, wantSignalErr) || !errors.Is(err, wantKillErr) {
		t.Fatalf("recovery Show() error = %v, want signal and kill failure", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("starts = %d, want no recovery retry", got)
	}
	launcher.signal = nil
	launcher.kill = nil
	stopLauncherForTest(t, launcher)
}

func TestProcessLauncherRecoveryPropagatesReplacementStartFailure(t *testing.T) {
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, func(start int32, command *exec.Cmd) {
		if start == 2 {
			command.Path = filepath.Join(t.TempDir(), "missing-ui")
			command.Args[0] = command.Path
		}
	})
	launcher.focus = func(context.Context) error { return errors.New("focus failed") }
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("first Show() error = %v", err)
	}
	err := launcher.Show(context.Background())
	if err == nil || !strings.Contains(err.Error(), "start desktop UI process") {
		t.Fatalf("recovery Show() error = %v, want replacement start failure", err)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("start attempts = %d, want exactly one replacement attempt", got)
	}
}

func TestProcessLauncherRecoveryPropagatesKillFailureAfterTimeout(t *testing.T) {
	wantKillErr := errors.New("kill failed")
	var starts atomic.Int32
	ready := make(chan struct{})
	launcher := newUIProcessTestLauncher(&starts, func(_ int32, command *exec.Cmd) {
		command.Env = append(command.Env, "REMOTE_DOCKER_UI_HELPER_ACTION=ignore-interrupt")
		command.Stdout = &readyWriter{ready: ready}
	})
	launcher.focus = func(context.Context) error { return errors.New("focus failed") }
	launcher.kill = func(*os.Process) error { return wantKillErr }
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("first Show() error = %v", err)
	}
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("helper did not become ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := launcher.Show(ctx)
	if !errors.Is(err, wantKillErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("recovery Show() error = %v, want timeout joined with kill failure", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("starts = %d after timeout, want no replacement", got)
	}
	launcher.kill = nil
	launcher.mu.Lock()
	command := launcher.process
	launcher.mu.Unlock()
	if command != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
	waitForLauncherChild(t, launcher)
}

func TestProcessLauncherStopPropagatesSignalAndKillFailure(t *testing.T) {
	wantSignalErr := errors.New("signal failed")
	wantKillErr := errors.New("kill failed")
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, nil)
	launcher.signal = func(*os.Process, os.Signal) error { return wantSignalErr }
	launcher.kill = func(*os.Process) error { return wantKillErr }
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("Show() error = %v", err)
	}
	err := launcher.Stop(context.Background())
	if !errors.Is(err, wantSignalErr) || !errors.Is(err, wantKillErr) {
		t.Fatalf("Stop() error = %v, want signal and kill failures", err)
	}
	launcher.signal = nil
	launcher.kill = nil
	stopLauncherForTest(t, launcher)
}

func TestProcessLauncherStopUsesExactKillFallbackAfterSignalFailure(t *testing.T) {
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, nil)
	launcher.signal = func(*os.Process, os.Signal) error { return errors.New("signal unsupported") }
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("Show() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := launcher.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v, want successful exact kill fallback", err)
	}
	if launcher.Running() {
		t.Fatal("UI child is still running after exact kill fallback")
	}
}

func TestProcessLauncherConcurrentStopsSignalExactChildOnce(t *testing.T) {
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, nil)
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("Show() error = %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	launcher.signal = func(process *os.Process, signal os.Signal) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return process.Signal(signal)
	}
	firstResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		firstResult <- launcher.Stop(ctx)
	}()
	<-entered
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	cancelSecond()
	if err := launcher.Stop(secondCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("second Stop() error = %v, want canceled while joining first stop", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("signal calls = %d, want exact child signaled once", got)
	}
	close(release)
	if err := <-firstResult; err != nil {
		t.Fatalf("first Stop() error = %v", err)
	}
}

func TestProcessLauncherShowFailsClosedWhileExactChildIsStopping(t *testing.T) {
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, nil)
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("Show() error = %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	launcher.signal = func(process *os.Process, signal os.Signal) error {
		close(entered)
		<-release
		return process.Signal(signal)
	}
	stopResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		stopResult <- launcher.Stop(ctx)
	}()
	<-entered
	var focusCalls atomic.Int32
	launcher.focus = func(context.Context) error {
		focusCalls.Add(1)
		return nil
	}
	if err := launcher.Show(context.Background()); !errors.Is(err, errUIProcessLauncherClosed) {
		t.Fatalf("Show() error = %v, want fail-closed launcher error", err)
	}
	if got := focusCalls.Load(); got != 0 {
		t.Fatalf("focus calls = %d while child stopping, want 0", got)
	}
	close(release)
	if err := <-stopResult; err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestProcessLauncherRecoveryTreatsExactNaturalExitAsSuccessfulStop(t *testing.T) {
	var starts atomic.Int32
	waitReturned := make(chan struct{})
	releaseCallback := make(chan struct{})
	var waits atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, nil)
	launcher.wait = func(command *exec.Cmd) error {
		err := command.Wait()
		if waits.Add(1) == 1 {
			close(waitReturned)
			<-releaseCallback
		}
		return err
	}
	launcher.focus = func(context.Context) error {
		launcher.mu.Lock()
		process := launcher.process.Process
		launcher.mu.Unlock()
		if err := process.Kill(); err != nil {
			t.Fatalf("end exact child: %v", err)
		}
		<-waitReturned
		return errors.New("focus lost during natural exit")
	}
	var releaseOnce sync.Once
	launcher.signal = func(*os.Process, os.Signal) error {
		releaseOnce.Do(func() { close(releaseCallback) })
		return os.ErrProcessDone
	}
	launcher.kill = func(*os.Process) error { return os.ErrProcessDone }
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("first Show() error = %v", err)
	}
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("recovery Show() error = %v, want natural exit replacement", err)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("starts = %d, want one replacement", got)
	}
	launcher.signal = nil
	launcher.kill = nil
	stopLauncherForTest(t, launcher)
}

func TestProcessLauncherStopTreatsExactNaturalExitAsSuccess(t *testing.T) {
	var starts atomic.Int32
	waitReturned := make(chan struct{})
	releaseCallback := make(chan struct{})
	launcher := newUIProcessTestLauncher(&starts, func(_ int32, command *exec.Cmd) {
		command.Env = append(command.Env, "REMOTE_DOCKER_UI_HELPER_ACTION=exit")
	})
	launcher.wait = func(command *exec.Cmd) error {
		err := command.Wait()
		close(waitReturned)
		<-releaseCallback
		return err
	}
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("Show() error = %v", err)
	}
	<-waitReturned
	var releaseOnce sync.Once
	launcher.signal = func(*os.Process, os.Signal) error {
		releaseOnce.Do(func() { close(releaseCallback) })
		return os.ErrProcessDone
	}
	launcher.kill = func(*os.Process) error { return os.ErrProcessDone }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := launcher.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v, want exact natural exit success", err)
	}
}

func TestProcessLauncherStopClosesLauncherAgainstFutureShow(t *testing.T) {
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, nil)
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("Show() error = %v", err)
	}
	stopLauncherForTest(t, launcher)
	if err := launcher.Show(context.Background()); !errors.Is(err, errUIProcessLauncherClosed) {
		t.Fatalf("Show() after terminal Stop error = %v, want launcher closed", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("starts = %d, want no start after terminal Stop", got)
	}
}

func TestProcessLauncherTerminalStopPreventsInflightRecoveryReplacement(t *testing.T) {
	var starts atomic.Int32
	ready := make(chan struct{})
	launcher := newUIProcessTestLauncher(&starts, func(_ int32, command *exec.Cmd) {
		command.Env = append(command.Env, "REMOTE_DOCKER_UI_HELPER_ACTION=ready")
		command.Stdout = &readyWriter{ready: ready}
	})
	focusEntered := make(chan struct{})
	releaseFocus := make(chan struct{})
	launcher.focus = func(context.Context) error {
		close(focusEntered)
		<-releaseFocus
		return errors.New("focus failed")
	}
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("first Show() error = %v", err)
	}
	<-ready
	showResult := make(chan error, 1)
	go func() { showResult <- launcher.Show(context.Background()) }()
	<-focusEntered
	stopResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		stopResult <- launcher.Stop(ctx)
	}()
	if err := <-stopResult; err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	close(releaseFocus)
	if err := <-showResult; !errors.Is(err, errUIProcessLauncherClosed) {
		t.Fatalf("in-flight Show() error = %v, want launcher closed", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("starts = %d, want no replacement after terminal Stop", got)
	}
}

func TestProcessLauncherFailedStopStaysClosedAndAllowsStopRetry(t *testing.T) {
	wantSignalErr := errors.New("signal failed")
	wantKillErr := errors.New("kill failed")
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, nil)
	launcher.signal = func(*os.Process, os.Signal) error { return wantSignalErr }
	launcher.kill = func(*os.Process) error { return wantKillErr }
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("Show() error = %v", err)
	}
	if err := launcher.Stop(context.Background()); !errors.Is(err, wantSignalErr) || !errors.Is(err, wantKillErr) {
		t.Fatalf("first Stop() error = %v, want signal and kill errors", err)
	}
	if err := launcher.Show(context.Background()); !errors.Is(err, errUIProcessLauncherClosed) {
		t.Fatalf("Show() after failed terminal Stop error = %v, want launcher closed", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("starts = %d, want no start after failed terminal Stop", got)
	}
	launcher.signal = nil
	launcher.kill = nil
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := launcher.Stop(ctx); err != nil {
		t.Fatalf("second Stop() retry error = %v", err)
	}
}

func TestProcessLauncherTerminalStopJoinsFailedRecoveryWithoutWaitingForChildExit(t *testing.T) {
	recoverySignalErr := errors.New("recovery signal failed")
	recoveryKillErr := errors.New("recovery kill failed")
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, nil)
	focusEntered := make(chan struct{})
	releaseFocus := make(chan struct{})
	launcher.focus = func(context.Context) error {
		close(focusEntered)
		<-releaseFocus
		return errors.New("focus failed")
	}
	recoverySignalEntered := make(chan struct{})
	releaseRecoverySignal := make(chan struct{})
	var signalCalls atomic.Int32
	launcher.signal = func(process *os.Process, signal os.Signal) error {
		switch signalCalls.Add(1) {
		case 1:
			close(recoverySignalEntered)
			<-releaseRecoverySignal
			return recoverySignalErr
		default:
			return process.Signal(signal)
		}
	}
	var killCalls atomic.Int32
	launcher.kill = func(process *os.Process) error {
		if killCalls.Add(1) == 1 {
			return recoveryKillErr
		}
		return process.Kill()
	}
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("first Show() error = %v", err)
	}
	showResult := make(chan error, 1)
	go func() { showResult <- launcher.Show(context.Background()) }()
	<-focusEntered
	close(releaseFocus)
	<-recoverySignalEntered
	stopResult := make(chan error, 1)
	go func() { stopResult <- launcher.Stop(context.Background()) }()
	close(releaseRecoverySignal)
	showErr := <-showResult
	if !errors.Is(showErr, recoverySignalErr) || !errors.Is(showErr, recoveryKillErr) {
		t.Fatalf("recovery Show() error = %v, want injected stop failures", showErr)
	}
	select {
	case err := <-stopResult:
		if err != nil {
			t.Fatalf("terminal Stop() error = %v, want one successful terminal attempt", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminal Stop() waited on live child instead of recovery operation")
	}
	if got := signalCalls.Load(); got != 2 {
		t.Fatalf("signal calls = %d, want one recovery and one terminal attempt", got)
	}
	if got := killCalls.Load(); got != 1 {
		t.Fatalf("kill calls = %d, want only failed recovery fallback", got)
	}
}

func TestProcessLauncherConcurrentStopJoinsFailedLeaderThenRetriesOnce(t *testing.T) {
	leaderSignalErr := errors.New("leader signal failed")
	leaderKillErr := errors.New("leader kill failed")
	var starts atomic.Int32
	launcher := newUIProcessTestLauncher(&starts, nil)
	leaderSignalEntered := make(chan struct{})
	releaseLeaderSignal := make(chan struct{})
	var signalCalls atomic.Int32
	launcher.signal = func(process *os.Process, signal os.Signal) error {
		switch signalCalls.Add(1) {
		case 1:
			close(leaderSignalEntered)
			<-releaseLeaderSignal
			return leaderSignalErr
		default:
			return process.Signal(signal)
		}
	}
	var killCalls atomic.Int32
	launcher.kill = func(process *os.Process) error {
		if killCalls.Add(1) == 1 {
			return leaderKillErr
		}
		return process.Kill()
	}
	if err := launcher.Show(context.Background()); err != nil {
		t.Fatalf("Show() error = %v", err)
	}
	leaderResult := make(chan error, 1)
	go func() { leaderResult <- launcher.Stop(context.Background()) }()
	<-leaderSignalEntered
	joinerResult := make(chan error, 1)
	go func() { joinerResult <- launcher.Stop(context.Background()) }()
	close(releaseLeaderSignal)
	leaderErr := <-leaderResult
	if !errors.Is(leaderErr, leaderSignalErr) || !errors.Is(leaderErr, leaderKillErr) {
		t.Fatalf("leader Stop() error = %v, want injected failures", leaderErr)
	}
	select {
	case err := <-joinerResult:
		if err != nil {
			t.Fatalf("joiner Stop() error = %v, want one successful retry", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("joiner Stop() waited on live child after leader failure")
	}
	if got := signalCalls.Load(); got != 2 {
		t.Fatalf("signal calls = %d, want one leader and one joiner attempt", got)
	}
	if got := killCalls.Load(); got != 1 {
		t.Fatalf("kill calls = %d, want only failed leader fallback", got)
	}
}

func TestUIProcessHelper(t *testing.T) {
	if os.Getenv("REMOTE_DOCKER_UI_HELPER") != "1" {
		return
	}
	if os.Getenv("REMOTE_DOCKER_UI_HELPER_ACTION") == "exit" {
		return
	}
	if os.Getenv("REMOTE_DOCKER_UI_HELPER_ACTION") == "ignore-interrupt" {
		signal.Ignore(os.Interrupt)
		fmt.Fprintln(os.Stdout, "ready")
		select {}
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	if os.Getenv("REMOTE_DOCKER_UI_HELPER_ACTION") == "ready" {
		fmt.Fprintln(os.Stdout, "ready")
	}
	<-interrupt
}

type readyWriter struct {
	once  sync.Once
	ready chan struct{}
}

func (w *readyWriter) Write(payload []byte) (int, error) {
	w.once.Do(func() { close(w.ready) })
	return len(payload), nil
}

func newUIProcessTestLauncher(starts *atomic.Int32, configure func(int32, *exec.Cmd)) *ProcessLauncher {
	return &ProcessLauncher{
		Executable: os.Args[0],
		Owner:      activeProcessOwner{},
		arguments:  []string{"-test.run=TestUIProcessHelper"},
		command: func(name string, args ...string) *exec.Cmd {
			start := starts.Add(1)
			command := exec.Command(name, args...)
			command.Env = append(os.Environ(), "REMOTE_DOCKER_UI_HELPER=1")
			if configure != nil {
				configure(start, command)
			}
			return command
		},
	}
}

func waitForLauncherChild(t *testing.T, launcher *ProcessLauncher) {
	t.Helper()
	launcher.mu.Lock()
	done := launcher.done
	launcher.mu.Unlock()
	if done == nil {
		t.Fatal("launcher has no child completion channel")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for UI child")
	}
}

func stopLauncherForTest(t *testing.T, launcher *ProcessLauncher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := launcher.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

type activeProcessOwner struct{}

func (activeProcessOwner) Active() bool { return true }

type inactiveProcessOwner struct{}

func (inactiveProcessOwner) Active() bool { return false }
