package app

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultRuntimeControlPathFitsMacOSUnixSocketLimit(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Unix socket path limit")
	}
	configPath := filepath.Join("/Users", "an-intentionally-long-user-name", "Library", "Application Support", "Remote Docker", "config.json")
	_, _, _, controlDir := defaultRuntimePaths(configPath)
	controlPath := filepath.Join(controlDir, "0123456789abcdef0123456789abcdef01234567")
	if len(controlPath) >= 104 {
		t.Fatalf("ControlPath length = %d (%q), want < 104", len(controlPath), controlPath)
	}
	if filepath.Dir(controlDir) != defaultPrivateRuntimeRoot() {
		t.Fatalf("control root = %q, want %q", filepath.Dir(controlDir), defaultPrivateRuntimeRoot())
	}
}
