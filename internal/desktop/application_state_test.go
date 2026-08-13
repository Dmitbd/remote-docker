package desktop

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	productassets "github.com/Dmitbd/remote-docker/internal/assets"
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
	quitCtx, quitCancel := context.WithTimeout(context.Background(), time.Second)
	if err := application.Quit(quitCtx); err != nil {
		t.Fatalf("Quit() error = %v", err)
	}
	quitCancel()
	cancel()
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
	iconCalls := tray.iconCallsSnapshot()
	if len(iconCalls) != 2 {
		t.Fatalf("icon calls = %d, want two", len(iconCalls))
	}
	if iconCalls[0].mode != "template" || iconCalls[1].mode != "template" {
		t.Fatalf("Darwin icon modes = %q, %q, want template", iconCalls[0].mode, iconCalls[1].mode)
	}
	if want := productassets.TrayIcon("darwin", productassets.TrayPaused); !bytes.Equal(iconCalls[0].icon, want) {
		t.Fatal("initial tray icon does not match paused state")
	}
	if want := productassets.TrayIcon("darwin", productassets.TrayConnected); !bytes.Equal(iconCalls[1].icon, want) {
		t.Fatal("updated tray icon does not match connected state")
	}
	quitCtx, quitCancel := context.WithTimeout(context.Background(), time.Second)
	if err := application.Quit(quitCtx); err != nil {
		t.Fatalf("Quit() error = %v", err)
	}
	quitCancel()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("application did not stop")
	}
}

func TestTrayApplicationUsesPlatformCorrectIconMode(t *testing.T) {
	for _, test := range []struct {
		platform string
		mode     string
	}{
		{platform: "windows", mode: "regular"},
		{platform: "darwin", mode: "template"},
		{platform: "linux", mode: "regular"},
	} {
		t.Run(test.platform, func(t *testing.T) {
			tray := newFakeTray()
			application, err := NewApplication(ApplicationOptions{
				UI:       &fakeUIProcess{},
				Snapshot: func() lifecycle.Snapshot { return lifecycle.Snapshot{} },
				Platform: test.platform,
				Tray:     tray,
			})
			if err != nil {
				t.Fatalf("NewApplication() error = %v", err)
			}

			application.updateTray(lifecycle.Snapshot{State: lifecycle.StatePaused})
			calls := tray.iconCallsSnapshot()
			if len(calls) != 1 {
				t.Fatalf("icon calls = %d, want one", len(calls))
			}
			if calls[0].mode != test.mode {
				t.Fatalf("icon mode = %q, want %q", calls[0].mode, test.mode)
			}
			want := productassets.TrayIcon(test.platform, productassets.TrayPaused)
			if !bytes.Equal(calls[0].icon, want) {
				t.Fatal("tray received bytes for the wrong platform/state")
			}
		})
	}
}

func TestApplicationShowPropagatesUIProcessError(t *testing.T) {
	wantErr := errors.New("focus failed")
	application, err := NewApplication(ApplicationOptions{
		UI: &fakeUIProcess{showErr: wantErr},
		Snapshot: func() lifecycle.Snapshot {
			return lifecycle.Snapshot{State: lifecycle.StatePaused}
		},
		Tray: newFakeTray(),
	})
	if err != nil {
		t.Fatalf("NewApplication() error = %v", err)
	}
	if err := application.Show(); !errors.Is(err, wantErr) {
		t.Fatalf("Show() error = %v, want %v", err, wantErr)
	}
}

type fakeUIProcess struct {
	showCalls atomic.Int32
	stopCalls atomic.Int32
	running   atomic.Bool
	showErr   error
}

func (p *fakeUIProcess) Show(context.Context) error {
	p.showCalls.Add(1)
	if p.showErr != nil {
		return p.showErr
	}
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
	iconMu    sync.Mutex
	iconCalls []trayIconCall
}

type trayIconCall struct {
	mode string
	icon []byte
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

func (t *fakeTray) SetRegularIcon(icon []byte) {
	t.recordIcon("regular", icon)
}
func (t *fakeTray) SetTemplateIcon(icon []byte) {
	t.recordIcon("template", icon)
}
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

func (t *fakeTray) recordIcon(mode string, icon []byte) {
	t.iconMu.Lock()
	t.iconCalls = append(t.iconCalls, trayIconCall{mode: mode, icon: append([]byte(nil), icon...)})
	t.iconMu.Unlock()
	t.icons.Add(1)
}

func (t *fakeTray) iconCallsSnapshot() []trayIconCall {
	t.iconMu.Lock()
	defer t.iconMu.Unlock()
	return append([]trayIconCall(nil), t.iconCalls...)
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
