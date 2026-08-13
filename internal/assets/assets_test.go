package assets

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"os"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
)

func TestEmbeddedAppAndDarwinTrayAssetsDecodeWithExpectedDimensions(t *testing.T) {
	app, err := png.Decode(bytes.NewReader(AppIcon()))
	if err != nil || app.Bounds().Dx() != 1024 || app.Bounds().Dy() != 1024 {
		t.Fatalf("app icon bounds=%v error=%v", app.Bounds(), err)
	}
	for _, state := range []TrayState{TrayPaused, TraySearch, TrayPairing, TrayConnected, TrayError} {
		got := TrayIcon("darwin", state)
		want, readErr := os.ReadFile("data/tray-darwin-" + string(state) + ".png")
		if readErr != nil {
			t.Fatalf("read Darwin tray %s: %v", state, readErr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Darwin tray %s differs from checked-in PNG", state)
		}
		icon, decodeErr := png.Decode(bytes.NewReader(got))
		if decodeErr != nil || icon.Bounds().Dx() != 32 || icon.Bounds().Dy() != 32 {
			t.Fatalf("tray darwin/%s bounds=%v error=%v", state, icon.Bounds(), decodeErr)
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
					t.Fatalf("tray darwin/%s has intermediate alpha %d", state, alpha)
				}
			}
		}
		if !transparent || !opaque {
			t.Fatalf("tray darwin/%s alpha transparent=%t opaque=%t", state, transparent, opaque)
		}
	}
}

func TestEmbeddedWindowsTrayAssetsUseMatchingICO(t *testing.T) {
	seen := make(map[string]TrayState)
	for _, state := range []TrayState{TrayPaused, TraySearch, TrayPairing, TrayConnected, TrayError} {
		got := TrayIcon("windows", state)
		want, err := os.ReadFile("data/tray-windows-" + string(state) + ".ico")
		if err != nil {
			t.Fatalf("read Windows tray %s: %v", state, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Windows tray %s differs from checked-in ICO", state)
		}
		if len(got) < 6 || binary.LittleEndian.Uint16(got[0:2]) != 0 ||
			binary.LittleEndian.Uint16(got[2:4]) != 1 || binary.LittleEndian.Uint16(got[4:6]) != 4 {
			t.Fatalf("Windows tray %s has invalid ICO header %x", state, got[:min(len(got), 6)])
		}
		key := string(got)
		if previous, ok := seen[key]; ok {
			t.Fatalf("Windows tray states %s and %s use identical ICO bytes", previous, state)
		}
		seen[key] = state
	}
}

func TestOtherPlatformsUseDecodableColoredPNGFallback(t *testing.T) {
	for _, state := range []TrayState{TrayPaused, TraySearch, TrayPairing, TrayConnected, TrayError} {
		want, err := os.ReadFile("../../assets/icon/tray/windows/" + string(state) + "-32.png")
		if err != nil {
			t.Fatalf("read colored tray fallback %s: %v", state, err)
		}
		for _, platform := range []string{"linux", "freebsd", "unsupported"} {
			got := TrayIcon(platform, state)
			if !bytes.Equal(got, want) {
				t.Fatalf("tray %s/%s differs from checked-in colored PNG fallback", platform, state)
			}
			if len(got) < 8 || !bytes.Equal(got[:8], []byte("\x89PNG\r\n\x1a\n")) {
				t.Fatalf("tray %s/%s is not PNG: %x", platform, state, got[:min(len(got), 8)])
			}
			icon, decodeErr := png.Decode(bytes.NewReader(got))
			if decodeErr != nil || icon.Bounds().Dx() != 32 || icon.Bounds().Dy() != 32 {
				t.Fatalf("tray %s/%s bounds=%v error=%v", platform, state, icon.Bounds(), decodeErr)
			}
		}
	}
	if got := TrayIcon("linux", TrayPaused); len(got) >= 4 && binary.LittleEndian.Uint16(got[2:4]) == 1 {
		t.Fatal("Linux tray fallback must not be an ICO")
	}
}

func TestTrayIconUnknownStateRetainsNilFallback(t *testing.T) {
	for _, platform := range []string{"darwin", "windows", "unsupported"} {
		if got := TrayIcon(platform, TrayState("unknown")); got != nil {
			t.Fatalf("unknown state for %s returned %d bytes, want nil", platform, len(got))
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
