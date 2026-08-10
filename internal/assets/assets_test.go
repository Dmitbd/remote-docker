package assets

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"os"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
)

func TestEmbeddedAppAndTrayAssetsDecodeWithExpectedDimensions(t *testing.T) {
	app, err := png.Decode(bytes.NewReader(AppIcon()))
	if err != nil || app.Bounds().Dx() != 1024 || app.Bounds().Dy() != 1024 {
		t.Fatalf("app icon bounds=%v error=%v", app.Bounds(), err)
	}
	for _, platform := range []string{"darwin", "windows"} {
		for _, state := range []TrayState{TrayPaused, TraySearch, TrayPairing, TrayConnected, TrayError} {
			icon, err := png.Decode(bytes.NewReader(TrayIcon(platform, state)))
			if err != nil || icon.Bounds().Dx() != 32 || icon.Bounds().Dy() != 32 {
				t.Fatalf("tray %s/%s bounds=%v error=%v", platform, state, icon.Bounds(), err)
			}
			transparent, opaque := false, false
			for y := 0; y < icon.Bounds().Dy(); y++ {
				for x := 0; x < icon.Bounds().Dx(); x++ {
					_, _, _, alpha := icon.At(x, y).RGBA()
					if alpha == 0 {
						transparent = true
					} else if alpha == 0xffff {
						opaque = true
					} else {
						t.Fatalf("tray %s/%s has intermediate alpha %d", platform, state, alpha)
					}
				}
			}
			if !transparent || !opaque {
				t.Fatalf("tray %s/%s alpha transparent=%t opaque=%t", platform, state, transparent, opaque)
			}
		}
	}
}

func TestPlatformContainersContainExpectedEntries(t *testing.T) {
	ico, err := os.ReadFile("../../assets/icon/app/remote-docker.ico")
	if err != nil || len(ico) < 6 || binary.LittleEndian.Uint16(ico[4:6]) != 8 {
		t.Fatalf("ICO entry count error=%v bytes=%d", err, len(ico))
	}
	icns, err := os.ReadFile("../../assets/icon/app/remote-docker.icns")
	if err != nil || len(icns) < 8 || string(icns[:4]) != "icns" || int(binary.BigEndian.Uint32(icns[4:8])) != len(icns) {
		t.Fatalf("ICNS header error=%v bytes=%d", err, len(icns))
	}
}

func TestEveryLifecycleStateMapsToATrayAsset(t *testing.T) {
	states := []lifecycle.State{
		lifecycle.StatePaused, lifecycle.StateClientReady, lifecycle.StateSearching, lifecycle.StateHostWaiting,
		lifecycle.StatePairing, lifecycle.StateConnecting, lifecycle.StateConnected, lifecycle.StateReconnecting,
		lifecycle.StateStopping, lifecycle.StateNeedsAction,
	}
	for _, state := range states {
		mapped := TrayStateFor(lifecycle.Snapshot{State: state})
		if len(TrayIcon("darwin", mapped)) == 0 || len(TrayIcon("windows", mapped)) == 0 {
			t.Fatalf("state %s maps to missing tray asset %s", state, mapped)
		}
	}
}
