package assets

import (
	"embed"
	"runtime"

	"github.com/Dmitbd/remote-docker/internal/lifecycle"
)

//go:embed data/*.png data/*.ico
var files embed.FS

type TrayState string

const (
	TrayPaused    TrayState = "paused"
	TraySearch    TrayState = "search"
	TrayPairing   TrayState = "pairing"
	TrayConnected TrayState = "connected"
	TrayError     TrayState = "error"
)

func AppIcon() []byte {
	return read("data/app.png")
}

func TrayIcon(platform string, state TrayState) []byte {
	if platform != "darwin" && platform != "windows" {
		platform = runtime.GOOS
	}
	if platform != "darwin" {
		platform = "windows"
	}
	extension := ".png"
	if platform == "windows" {
		extension = ".ico"
	}
	return read("data/tray-" + platform + "-" + string(state) + extension)
}

func TrayStateFor(snapshot lifecycle.Snapshot) TrayState {
	if snapshot.Problem != nil || snapshot.State == lifecycle.StateNeedsAction {
		return TrayError
	}
	switch snapshot.State {
	case lifecycle.StatePaused, lifecycle.StateStopping:
		return TrayPaused
	case lifecycle.StatePairing:
		return TrayPairing
	case lifecycle.StateConnecting, lifecycle.StateConnected:
		return TrayConnected
	default:
		return TraySearch
	}
}

func read(name string) []byte {
	data, err := files.ReadFile(name)
	if err != nil {
		return nil
	}
	return append([]byte(nil), data...)
}
