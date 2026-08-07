package pairing

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"
)

const (
	// MaxSessionTTL is the longest permitted pairing window.
	MaxSessionTTL = 120 * time.Second
	maxAttempts   = 5
)

var (
	ErrSessionActive  = errors.New("a pairing session is already active")
	ErrInvalidSession = errors.New("invalid pairing session")
)

// ServerIdentity is an ephemeral Ed25519 identity used for pairing and TLS.
type ServerIdentity struct {
	PrivateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
}

// PublicKey returns a private copy of the identity's public key.
func (i ServerIdentity) PublicKey() ed25519.PublicKey {
	publicKey := i.publicKey
	if len(publicKey) == 0 && len(i.PrivateKey) == ed25519.PrivateKeySize {
		publicKey = i.PrivateKey.Public().(ed25519.PublicKey)
	}
	return append(ed25519.PublicKey(nil), publicKey...)
}

// SessionDescriptor contains the public data needed to compare pairing codes.
type SessionDescriptor struct {
	ID              string            `json:"id"`
	Nonce           []byte            `json:"nonce"`
	ServerPublicKey ed25519.PublicKey `json:"server_public_key"`
	ClientPublicKey ed25519.PublicKey `json:"client_public_key"`
	ExpiresAt       time.Time         `json:"expires_at"`
}

// DeviceInfo contains public identifiers returned after successful pairing.
type DeviceInfo struct {
	SSHHostPublicKey  string `json:"ssh_host_public_key"`
	SyncthingDeviceID string `json:"syncthing_device_id"`
	SSHPort           int    `json:"ssh_port"`
	SyncthingPort     int    `json:"syncthing_port"`
}

// DeviceRecord is the public result of a successful pairing.
type DeviceRecord struct {
	DeviceID          string   `json:"device_id"`
	AuthorizedKeys    []string `json:"authorized_keys"`
	SSHHostPublicKey  string   `json:"ssh_host_public_key"`
	SyncthingDeviceID string   `json:"syncthing_device_id"`
	SSHPort           int      `json:"ssh_port"`
	SyncthingPort     int      `json:"syncthing_port"`
}

// Code calculates the six-digit out-of-band comparison code.
func Code(descriptor SessionDescriptor) (string, error) {
	if len(descriptor.Nonce) == 0 ||
		len(descriptor.ServerPublicKey) != ed25519.PublicKeySize ||
		len(descriptor.ClientPublicKey) != ed25519.PublicKeySize {
		return "", ErrInvalidSession
	}

	mac := hmac.New(sha256.New, descriptor.Nonce)
	_, _ = mac.Write(descriptor.ServerPublicKey)
	_, _ = mac.Write(descriptor.ClientPublicKey)
	digest := mac.Sum(nil)
	firstTwentyBits := uint32(digest[0])<<12 | uint32(digest[1])<<4 | uint32(digest[2])>>4
	return fmt.Sprintf("%06d", firstTwentyBits%1_000_000), nil
}
