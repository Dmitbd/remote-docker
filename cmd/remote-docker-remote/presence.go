package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

var (
	errPresenceSession  = errors.New("presence session is unavailable")
	errPresenceSequence = errors.New("presence heartbeat sequence is invalid")
)

type presenceHelloParams struct {
	ClientDeviceID string `json:"client_device_id"`
	ClientName     string `json:"client_name"`
	AppVersion     string `json:"app_version"`
}

type presenceHeartbeatParams struct {
	SessionID string `json:"session_id"`
	Sequence  uint64 `json:"monotonic_sequence"`
}

type presenceDisconnectParams struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
}

type presenceResult struct {
	SessionID         string `json:"session_id,omitempty"`
	ServerMonotonicMS int64  `json:"server_monotonic_ms"`
	DockerReady       bool   `json:"docker_ready"`
	SyncReady         bool   `json:"sync_ready"`
	Terminal          bool   `json:"terminal,omitempty"`
	Reason            string `json:"reason,omitempty"`
}

type presenceOptions struct {
	Now    func() time.Time
	Random io.Reader
	Ready  func(context.Context) (docker, sync bool)
}

type presenceLease struct {
	deviceID       string
	name           string
	version        string
	sessionID      string
	sequence       uint64
	lastSeen       time.Time
	terminalReason string
}

type presenceManager struct {
	mu      sync.Mutex
	now     func() time.Time
	random  io.Reader
	ready   func(context.Context) (bool, bool)
	started time.Time
	lease   *presenceLease
}

func newPresenceManager(options presenceOptions) *presenceManager {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	random := options.Random
	if random == nil {
		random = rand.Reader
	}
	return &presenceManager{now: now, random: random, ready: options.Ready, started: now()}
}

func (m *presenceManager) Hello(ctx context.Context, params presenceHelloParams) (presenceResult, error) {
	if !validPresenceText(params.ClientDeviceID, 128) || !validPresenceText(params.ClientName, 64) ||
		!validPresenceText(params.AppVersion, 64) {
		return presenceResult{}, errPresenceSession
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lease != nil {
		return presenceResult{}, errPresenceSession
	}
	raw := make([]byte, 16)
	if _, err := io.ReadFull(m.random, raw); err != nil {
		return presenceResult{}, errPresenceSession
	}
	m.lease = &presenceLease{
		deviceID: params.ClientDeviceID, name: params.ClientName, version: params.AppVersion,
		sessionID: hex.EncodeToString(raw), lastSeen: m.now(),
	}
	return m.resultLocked(ctx, m.lease.sessionID), nil
}

func (m *presenceManager) Heartbeat(ctx context.Context, params presenceHeartbeatParams) (presenceResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lease == nil || params.SessionID != m.lease.sessionID {
		return presenceResult{}, errPresenceSession
	}
	if params.Sequence <= m.lease.sequence {
		return presenceResult{}, errPresenceSequence
	}
	m.lease.sequence = params.Sequence
	m.lease.lastSeen = m.now()
	result := m.resultLocked(ctx, m.lease.sessionID)
	if m.lease.terminalReason != "" {
		result.Terminal = true
		result.Reason = m.lease.terminalReason
		m.lease = nil
	}
	return result, nil
}

func (m *presenceManager) Disconnect(_ context.Context, params presenceDisconnectParams) error {
	if !validDisconnectReason(params.Reason) {
		return errPresenceSession
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lease == nil || params.SessionID != m.lease.sessionID {
		return errPresenceSession
	}
	m.lease = nil
	return nil
}

func (m *presenceManager) RequestTerminal(sessionID, reason string) error {
	if !validDisconnectReason(reason) {
		return errPresenceSession
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lease == nil || sessionID != m.lease.sessionID {
		return errPresenceSession
	}
	m.lease.terminalReason = reason
	return nil
}

func (m *presenceManager) resultLocked(ctx context.Context, sessionID string) presenceResult {
	dockerReady, syncReady := false, false
	if m.ready != nil {
		dockerReady, syncReady = m.ready(ctx)
	}
	return presenceResult{
		SessionID: sessionID, ServerMonotonicMS: m.now().Sub(m.started).Milliseconds(),
		DockerReady: dockerReady, SyncReady: syncReady,
	}
}

func validPresenceText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00")
}

func validDisconnectReason(reason string) bool {
	switch reason {
	case "user_disconnect", "user_pause", "peer_quit", "windows_disconnect", "network_timeout", "runtime_failure":
		return true
	default:
		return false
	}
}
