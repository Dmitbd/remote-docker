package main

import (
	"context"
	"io/fs"
	"reflect"
	"sort"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/desktopui"
)

func TestWindowOptionsUseApprovedWailsContract(t *testing.T) {
	bridge := &UIBridge{backend: &recordingUIBackend{}}
	configured := windowOptions(bridge)
	if configured.Title != "Remote Docker" || configured.Width != 1050 || configured.Height != 720 ||
		configured.MinWidth != 760 || configured.MinHeight != 580 || configured.HideWindowOnClose ||
		configured.AssetServer == nil || configured.AssetServer.Assets == nil {
		t.Fatalf("window options = %#v", configured)
	}
	if len(configured.Bind) != 1 || configured.Bind[0] != bridge {
		t.Fatalf("bound values = %#v, want only UI bridge", configured.Bind)
	}
	if configured.Windows == nil || configured.Mac == nil || configured.DragAndDrop == nil ||
		!configured.DragAndDrop.DisableWebViewDrop {
		t.Fatalf("platform options = Windows:%#v Mac:%#v Drag:%#v", configured.Windows, configured.Mac, configured.DragAndDrop)
	}
}

func TestUIBridgeExportsOnlyApprovedMethods(t *testing.T) {
	typeOfBridge := reflect.TypeOf(&UIBridge{})
	methods := make([]string, 0, typeOfBridge.NumMethod())
	for index := 0; index < typeOfBridge.NumMethod(); index++ {
		methods = append(methods, typeOfBridge.Method(index).Name)
	}
	sort.Strings(methods)
	want := []string{"Perform", "PickWorkspace", "Quit", "Snapshot"}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("exported bridge methods = %v, want %v", methods, want)
	}
}

func TestEmbeddedFrontendContainsProductionAssets(t *testing.T) {
	assets := frontendAssets()
	for _, path := range []string{"index.html", "styles.css", "app.js", "assets/app.png", "assets/icons.svg"} {
		info, err := fs.Stat(assets, path)
		if err != nil || info.IsDir() || info.Size() == 0 {
			t.Fatalf("embedded asset %q info=%#v error=%v", path, info, err)
		}
	}
}

func TestProductionBuildRejectsMockArguments(t *testing.T) {
	if _, enabled, err := mockBackendFromArgs([]string{"--mock=mac:connected"}, "darwin"); err == nil || enabled {
		t.Fatalf("production mock arguments enabled=%t error=%v", enabled, err)
	}
}

func TestUIBridgeDelegatesStateWithoutOwningRuntime(t *testing.T) {
	backend := &recordingUIBackend{state: desktopui.State{Revision: 11, Lifecycle: "connected"}}
	bridge := &UIBridge{backend: backend}
	bridge.startup(context.Background())
	state, err := bridge.Snapshot()
	if err != nil || state.Revision != 11 || backend.snapshots != 1 {
		t.Fatalf("Snapshot() state=%#v calls=%d error=%v", state, backend.snapshots, err)
	}
	state, err = bridge.Perform(desktopui.OperationPause, "")
	if err != nil || backend.operation != desktopui.OperationPause {
		t.Fatalf("Perform() state=%#v operation=%q error=%v", state, backend.operation, err)
	}
}

type recordingUIBackend struct {
	state     desktopui.State
	snapshots int
	operation string
}

func (b *recordingUIBackend) Snapshot(context.Context) (desktopui.State, error) {
	b.snapshots++
	return b.state, nil
}

func (b *recordingUIBackend) Perform(_ context.Context, id, _ string) (desktopui.State, error) {
	b.operation = id
	return b.state, nil
}

func (b *recordingUIBackend) Quit(context.Context) error { return nil }
