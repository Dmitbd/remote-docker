//go:build devui

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Dmitbd/remote-docker/internal/desktopui"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/metrics"
)

var approvedMockStates = map[string]struct {
	role  string
	state string
}{
	"mac:paused":           {"mac_client", "paused"},
	"mac:ready":            {"mac_client", "client_ready"},
	"mac:searching":        {"mac_client", "searching"},
	"mac:pairing":          {"mac_client", "pairing"},
	"mac:connecting":       {"mac_client", "connecting"},
	"mac:connected":        {"mac_client", "connected"},
	"mac:reconnecting":     {"mac_client", "reconnecting"},
	"mac:attention":        {"mac_client", "needs_action"},
	"windows:paused":       {"windows_host", "paused"},
	"windows:waiting":      {"windows_host", "host_waiting"},
	"windows:pairing":      {"windows_host", "pairing"},
	"windows:connecting":   {"windows_host", "connecting"},
	"windows:connected":    {"windows_host", "connected"},
	"windows:reconnecting": {"windows_host", "reconnecting"},
}

type mockUIBackend struct {
	mu       sync.Mutex
	platform string
	role     string
	state    string
	revision uint64
}

func mockBackendFromArgs(args []string, platform string) (uiBackend, bool, error) {
	if len(args) == 0 {
		return nil, false, nil
	}
	if len(args) != 1 || !strings.HasPrefix(args[0], "--mock=") {
		return nil, false, errors.New("devui expects --mock=<platform:state>")
	}
	name := strings.TrimPrefix(args[0], "--mock=")
	scenario, ok := approvedMockStates[name]
	if !ok {
		return nil, false, fmt.Errorf("unsupported devui state %q", name)
	}
	if strings.HasPrefix(name, "windows:") {
		platform = "windows"
	} else {
		platform = "darwin"
	}
	return &mockUIBackend{platform: platform, role: scenario.role, state: scenario.state, revision: 1}, true, nil
}

func (b *mockUIBackend) Snapshot(context.Context) (desktopui.State, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshot(), nil
}

func (b *mockUIBackend) Perform(_ context.Context, id, _ string) (desktopui.State, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch id {
	case desktopui.OperationEnableClient:
		b.state = "client_ready"
	case desktopui.OperationEnableHost:
		b.state = "host_waiting"
	case desktopui.OperationStartSearch:
		b.state = "searching"
	case desktopui.OperationStopSearch:
		b.state = "client_ready"
	case desktopui.OperationConnect, desktopui.OperationConnectTrusted:
		b.state = "pairing"
	case desktopui.OperationApprovePair:
		b.state = "connecting"
	case desktopui.OperationRejectPair:
		b.state = "host_waiting"
	case desktopui.OperationCancelPair:
		b.state = "searching"
	case desktopui.OperationPause:
		b.state = "paused"
	case desktopui.OperationDisconnect:
		if b.role == "windows_host" {
			b.state = "host_waiting"
		} else {
			b.state = "client_ready"
		}
	case desktopui.OperationDiagnostics, desktopui.OperationAddProject, desktopui.OperationRemoveProject:
	default:
		return desktopui.State{}, errors.New("действие недоступно в devui")
	}
	b.revision++
	return b.snapshot(), nil
}

func (b *mockUIBackend) Quit(context.Context) error { return nil }

func (b *mockUIBackend) snapshot() desktopui.State {
	status := localapi.StatusResult{
		Revision: b.revision, Role: b.role, State: b.state, LocalName: "Это устройство",
		ConnectionLimit: 1, TrustedPeers: 1,
		Peer:   &localapi.LifecyclePeer{ID: "peer", Name: peerName(b.role)},
		Docker: localapi.ServiceStatus{State: "ready"}, Sync: localapi.ServiceStatus{State: "ready", LastSuccess: "только что"},
		LatencyMS: 8,
	}
	if b.state == "pairing" {
		status.Pairing = &localapi.PairingStatusResult{SessionID: "devui", Peer: *status.Peer, Code: "381604", Status: "pending"}
	}
	if b.state == "reconnecting" {
		status.Recovery = &localapi.RecoveryStatus{Deadline: time.Now().Add(6 * time.Second).Format(time.RFC3339Nano)}
		status.LastDisconnect = &localapi.DisconnectStatus{Initiator: "peer", Reason: "network_timeout"}
	}
	if b.state == "needs_action" {
		status.Problem = &localapi.ProblemStatus{Code: "lan_blocked", Message: "Локальная сеть недоступна.", Action: "Разрешите локальную сеть для Remote Docker."}
	}
	input := desktopui.SnapshotInput{
		Status: status,
		Candidates: []localapi.PairingCandidate{
			{ID: "peer", Name: peerName(b.role), Trusted: true, Available: true},
			{ID: "new-peer", Name: "Другой Windows", Available: true, Unverified: true},
		},
		Workspaces: []localapi.Workspace{{ID: "project", Name: "demo-project", Path: "/Users/demo/Projects/demo-project"}},
		Sync:       localapi.SyncStatusResult{Folders: []localapi.SyncFolderStatus{{WorkspaceID: "project", State: "ready", Connected: true, LastSuccess: "только что"}}},
		Resources: localapi.ResourceStatusResult{
			At:                  time.Now(),
			MacRemoteDocker:     metrics.ProcessUsage{CPUPercent: metrics.Available(1.4), MemoryBytes: metrics.Available(uint64(146 << 20))},
			WindowsRemoteDocker: metrics.ProcessUsage{CPUPercent: metrics.Available(2.1), MemoryBytes: metrics.Available(uint64(220 << 20))},
			WindowsManagedWSL:   metrics.ProcessUsage{CPUPercent: metrics.Available(18.0), MemoryBytes: metrics.Available(uint64(68 << 29))},
			DockerContainers:    metrics.Available(12), ManagedDiskBytes: metrics.Available(uint64(42 << 30)),
			SyncNetwork: metrics.Rate{Available: true, BytesPerSecond: 18 << 20},
		},
		Diagnostics: []localapi.DoctorCheck{
			{Name: "lan_reachability", OK: true}, {Name: "tunnel_identity", OK: true},
			{Name: "tunnel_session", OK: b.state == "connected"}, {Name: "docker_channel", OK: b.state == "connected"},
			{Name: "sync_channel", Status: "running"}, {Name: "managed_wsl", OK: b.role == "windows_host" || b.state == "connected"},
		},
	}
	return desktopui.BuildState(input, b.platform, time.Now())
}

func peerName(role string) string {
	if role == "windows_host" {
		return "MacBook"
	}
	return "Windows PC"
}
