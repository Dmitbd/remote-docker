package provision

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// DistroProbe reports whether the reserved WSL name belongs to this app.
type DistroProbe struct {
	Exists        bool `json:"exists"`
	MarkerMatches bool `json:"marker_matches"`
}

// ProbeResult is the read-only JSON contract returned by probe.ps1.
type ProbeResult struct {
	WindowsBuild          int         `json:"windows_build"`
	VirtualizationEnabled bool        `json:"virtualization_enabled"`
	WSLInstalled          bool        `json:"wsl_installed"`
	WSL2Ready             bool        `json:"wsl2_ready"`
	Distro                DistroProbe `json:"distro"`
	FreeBytes             uint64      `json:"free_bytes"`
	FirewallCapability    bool        `json:"firewall_capability"`
}

// DecodeProbeResult strictly decodes one PowerShell probe result.
func DecodeProbeResult(reader io.Reader) (ProbeResult, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var result ProbeResult
	if err := decoder.Decode(&result); err != nil {
		return ProbeResult{}, fmt.Errorf("decode Windows probe result: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return ProbeResult{}, errors.New("decode Windows probe result: trailing JSON value")
		}
		return ProbeResult{}, fmt.Errorf("decode Windows probe trailing data: %w", err)
	}
	return result, nil
}
