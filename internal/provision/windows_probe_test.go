package provision

import (
	"strings"
	"testing"
)

func TestDecodeProbeResult(t *testing.T) {
	input := `{
  "windows_build": 22631,
  "virtualization_enabled": true,
  "wsl_installed": true,
  "wsl2_ready": true,
  "distro": {"exists": true, "marker_matches": true},
  "free_bytes": 85899345920,
  "firewall_capability": true
}`
	result, err := DecodeProbeResult(strings.NewReader(input))
	if err != nil {
		t.Fatalf("DecodeProbeResult() error = %v", err)
	}
	if result.WindowsBuild != 22631 || !result.WSL2Ready || !result.Distro.MarkerMatches {
		t.Fatalf("DecodeProbeResult() = %#v", result)
	}
}

func TestDecodeProbeResultRejectsUnknownOrTrailingJSON(t *testing.T) {
	for _, input := range []string{
		`{"windows_build":22631,"unknown":true}`,
		`{"windows_build":22631} {}`,
	} {
		if _, err := DecodeProbeResult(strings.NewReader(input)); err == nil {
			t.Fatalf("DecodeProbeResult(%q) succeeded, want error", input)
		}
	}
}
