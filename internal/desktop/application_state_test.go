package desktop

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
)

func TestTrayApplicationLaunchesWindowAndUsesOneCompleteShutdownPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tray := newFakeTray()
	ui := &fakeUIProcess{}
	var pauses atomic.Int32
	var quits atomic.Int32
	application, err := NewApplication(ApplicationOptions{
		UI: ui,
		Snapshot: func() lifecycle.Snapshot {
			return lifecycle.Snapshot{Role: lifecycle.RoleMacClient, State: lifecycle.StateClientReady}
		},
		Tray: tray,
		OnPause: func(context.Context) error {
			pauses.Add(1)
			return nil
		},
		OnQuit: func(context.Context) error {
			quits.Add(1)
			cancel()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewApplication() error = %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- application.Run(ctx) }()
	tray.waitReady(t)
	if ui.showCalls.Load() != 1 {
		t.Fatalf("initial UI Show calls = %d, want one", ui.showCalls.Load())
	}
	tray.click("Открыть Remote Docker")
	waitAtomic(t, &ui.showCalls, 2, "tray open")
	tray.click("Поставить на паузу")
	waitAtomic(t, &pauses, 1, "tray pause")
	tray.click("Завершить работу")
	waitAtomic(t, &quits, 1, "tray quit")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tray application did not stop")
	}
	if ui.stopCalls.Load() != 1 || tray.quitCalls.Load() != 1 {
		t.Fatalf("cleanup UI=%d tray=%d", ui.stopCalls.Load(), tray.quitCalls.Load())
	}
}

func TestTrayApplicationReflectsLifecycleIconUpdates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	updates := make(chan lifecycle.Snapshot, 1)
	tray := newFakeTray()
	application, err := NewApplication(ApplicationOptions{
		UI: &fakeUIProcess{}, Snapshot: func() lifecycle.Snapshot {
			return lifecycle.Snapshot{State: lifecycle.StatePaused}
		},
		Updates: updates, Tray: tray, Platform: "darwin",
	})
	if err != nil {
		t.Fatalf("NewApplication() error = %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- application.Run(ctx) }()
	tray.waitReady(t)
	updates <- lifecycle.Snapshot{State: lifecycle.StateConnected}
	tray.waitIcons(t, 2)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("application did not stop")
	}
}

type fakeUIProcess struct {
	showCalls atomic.Int32
	stopCalls atomic.Int32
	running   atomic.Bool
}

func (p *fakeUIProcess) Show(context.Context) error {
	p.showCalls.Add(1)
	p.running.Store(true)
	return nil
}

func (p *fakeUIProcess) Stop(context.Context) error {
	p.stopCalls.Add(1)
	p.running.Store(false)
	return nil
}

func (p *fakeUIProcess) Running() bool { return p.running.Load() }

type fakeTray struct {
	mu        sync.Mutex
	items     map[string]*fakeTrayItem
	ready     chan struct{}
	done      chan struct{}
	quitOnce  sync.Once
	quitCalls atomic.Int32
	icons     atomic.Int32
}

func newFakeTray() *fakeTray {
	return &fakeTray{items: make(map[string]*fakeTrayItem), ready: make(chan struct{}), done: make(chan struct{})}
}

func (t *fakeTray) Run(onReady, onExit func()) {
	onReady()
	close(t.ready)
	<-t.done
	onExit()
}

func (t *fakeTray) Quit() {
	t.quitCalls.Add(1)
	t.quitOnce.Do(func() { close(t.done) })
}

func (t *fakeTray) SetIcon([]byte)    { t.icons.Add(1) }
func (t *fakeTray) SetTooltip(string) {}
func (t *fakeTray) AddSeparator()     {}
func (t *fakeTray) AddMenuItem(title, _ string) trayMenuItem {
	t.mu.Lock()
	defer t.mu.Unlock()
	item := &fakeTrayItem{clicked: make(chan struct{}, 2)}
	t.items[title] = item
	return item
}

func (t *fakeTray) waitReady(testingT *testing.T) {
	testingT.Helper()
	select {
	case <-t.ready:
	case <-time.After(2 * time.Second):
		testingT.Fatal("tray did not become ready")
	}
}

func (t *fakeTray) click(title string) {
	t.mu.Lock()
	item := t.items[title]
	t.mu.Unlock()
	item.clicked <- struct{}{}
}

func (t *fakeTray) waitIcons(testingT *testing.T, count int32) {
	testingT.Helper()
	deadline := time.After(2 * time.Second)
	for t.icons.Load() < count {
		select {
		case <-deadline:
			testingT.Fatalf("tray icon updates = %d, want %d", t.icons.Load(), count)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

type fakeTrayItem struct{ clicked chan struct{} }

func (i *fakeTrayItem) Clicked() <-chan struct{} { return i.clicked }

func waitAtomic(t *testing.T, value *atomic.Int32, want int32, label string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for value.Load() < want {
		select {
		case <-deadline:
			t.Fatalf("%s calls = %d, want %d", label, value.Load(), want)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}
