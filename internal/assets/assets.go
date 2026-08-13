package assets

import (
	"embed"

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
	switch platform {
	case "darwin":
		return read("data/tray-darwin-" + string(state) + ".png")
	case "windows":
		return read("data/tray-windows-" + string(state) + ".ico")
	default:
		return read("data/tray-windows-" + string(state) + ".png")
	}
}

func TrayStateFor(snapshot lifecycle.Snapshot) TrayState {
	if snapshot.Problem != nil || snapshot.State == lifecycle.StateNeedsAction {
		return TrayError
	}
	switch snapshot.State {
	case lifecycle.StatePaused, lifecycle.StateStopping:
		return TrayPaused
	case lifecycle.StatePairing, lifecycle.StatePairingCancellationPending:
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
