package main

import (
	"context"
	"encoding/binary"
	"image/color"
	"reflect"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/tray"
)

func TestAddWorkspaceActionUsesDirectoryPickerWithoutCLIFlags(t *testing.T) {
	t.Parallel()

	client := &trayClient{results: map[localapi.Method]any{
		localapi.MethodWorkspaceAdd: localapi.Workspace{ID: "workspace-1", Path: "/selected/workspace"},
	}}
	picker := &recordingDirectoryPicker{path: "/selected/workspace"}
	presentation := &presentation{controller: tray.NewController(client), picker: picker}

	presentation.invoke(tray.ActionAddWorkspace)
	if picker.calls != 1 {
		t.Fatalf("picker calls = %d, want 1", picker.calls)
	}
	params, ok := client.lastParams(localapi.MethodWorkspaceAdd).(localapi.WorkspaceAddParams)
	if !ok || params.Path != "/selected/workspace" {
		t.Fatalf("WorkspaceAdd params = %#v", params)
	}
}

func TestPairActionDiscoversCandidatesWithoutCLIFlags(t *testing.T) {
	t.Parallel()

	want := []localapi.PairingCandidate{{ID: "peer-1", Name: "Windows PC"}, {ID: "peer-2", Name: "Gaming PC"}}
	client := &trayClient{results: map[localapi.Method]any{
		localapi.MethodPairCandidates: localapi.PairCandidatesResult{Candidates: want},
	}}
	presentation := &presentation{controller: tray.NewController(client)}
	presentation.invoke(tray.ActionPair)

	if got := presentation.controller.Current().Candidates; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %#v, want %#v", got, want)
	}
}

func TestWindowsPairActionShowsActiveMacPairingCode(t *testing.T) {
	t.Parallel()

	client := &trayClient{results: map[localapi.Method]any{
		localapi.MethodPairStart: localapi.PairStartResult{SessionID: "session-1", Code: "654321"},
	}}
	presentation := &presentation{controller: tray.NewController(client), platform: "windows"}
	presentation.invoke(tray.ActionPair)

	model := presentation.controller.Current()
	if model.Pairing == nil || model.Pairing.DeviceName != "Mac device" || model.Pairing.Code != "654321" {
		t.Fatalf("pairing = %#v, want active Mac session and six-digit code", model.Pairing)
	}
	if client.called(localapi.MethodPairCandidates) {
		t.Fatal("Windows Pair action called PairCandidates, want PairStart")
	}
}

func TestWindowsPairingConfirmationStaysOnMac(t *testing.T) {
	t.Parallel()

	label, enabled := pairingConfirmation("windows")
	if label != "Confirm on Mac" || enabled {
		t.Fatalf("pairingConfirmation(windows) = %q, %t, want Confirm on Mac, false", label, enabled)
	}
}

func TestICOFromPNGHasValidSingleImageDirectory(t *testing.T) {
	t.Parallel()

	pngBytes := statusPNG(color.RGBA{R: 37, G: 145, B: 70, A: 255})
	ico := icoFromPNG(pngBytes)
	if len(ico) <= 22 || binary.LittleEndian.Uint16(ico[0:2]) != 0 ||
		binary.LittleEndian.Uint16(ico[2:4]) != 1 || binary.LittleEndian.Uint16(ico[4:6]) != 1 {
		t.Fatalf("ICO header = % x", ico[:min(len(ico), 22)])
	}
	if ico[6] != 16 || ico[7] != 16 || binary.LittleEndian.Uint32(ico[18:22]) != 22 {
		t.Fatalf("ICO directory = % x", ico[6:22])
	}
	if got := binary.LittleEndian.Uint32(ico[14:18]); got != uint32(len(pngBytes)) {
		t.Fatalf("ICO image length = %d, want %d", got, len(pngBytes))
	}
}

type recordingDirectoryPicker struct {
	path  string
	calls int
}

func (p *recordingDirectoryPicker) Choose(context.Context) (string, error) {
	p.calls++
	return p.path, nil
}

type trayClient struct {
	results map[localapi.Method]any
	calls   []trayCall
}

type trayCall struct {
	method localapi.Method
	params any
}

func (c *trayClient) Call(_ context.Context, method localapi.Method, params, destination any) error {
	c.calls = append(c.calls, trayCall{method: method, params: params})
	result, ok := c.results[method]
	if !ok || destination == nil {
		return nil
	}
	reflect.ValueOf(destination).Elem().Set(reflect.ValueOf(result))
	return nil
}

func (c *trayClient) lastParams(method localapi.Method) any {
	for index := len(c.calls) - 1; index >= 0; index-- {
		if c.calls[index].method == method {
			return c.calls[index].params
		}
	}
	return nil
}

func (c *trayClient) called(method localapi.Method) bool {
	for _, call := range c.calls {
		if call.method == method {
			return true
		}
	}
	return false
}
