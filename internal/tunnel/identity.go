package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/Dmitbd/remote-docker/internal/credentials"
)

const (
	WindowsIdentityOwner    = "windows-host"
	IdentityCredential      = "tunnel-ed25519-private-key"
	CurrentTransportVersion = 1
	TunnelPort              = 49221
)

type Identity struct {
	PrivateKey ed25519.PrivateKey
	PublicKey  ed25519.PublicKey
}

func LoadOrCreateIdentity(store credentials.Store, owner string) (Identity, error) {
	if store == nil || owner == "" {
		return Identity{}, errors.New("tunnel identity store and owner are required")
	}
	encoded, err := store.Get(owner, IdentityCredential)
	if err == nil {
		defer zero(encoded)
		return IdentityFromPKCS8(encoded)
	}
	if !errors.Is(err, credentials.ErrNotFound) {
		return Identity{}, fmt.Errorf("load tunnel identity: %w", err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, fmt.Errorf("generate tunnel identity: %w", err)
	}
	identity := Identity{PrivateKey: privateKey, PublicKey: publicKey}
	encoded, err = EncodeIdentity(identity)
	if err != nil {
		return Identity{}, err
	}
	defer zero(encoded)
	if err := store.Put(owner, IdentityCredential, encoded); err != nil {
		return Identity{}, fmt.Errorf("store tunnel identity: %w", err)
	}
	return cloneIdentity(identity), nil
}

func IdentityFromPKCS8(encoded []byte) (Identity, error) {
	parsed, err := x509.ParsePKCS8PrivateKey(encoded)
	if err != nil {
		return Identity{}, fmt.Errorf("parse tunnel identity: %w", err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		return Identity{}, errors.New("tunnel identity is not an Ed25519 private key")
	}
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize ||
		subtle.ConstantTimeCompare(privateKey[ed25519.SeedSize:], publicKey) != 1 {
		return Identity{}, errors.New("tunnel identity public and private keys do not match")
	}
	return cloneIdentity(Identity{PrivateKey: privateKey, PublicKey: publicKey}), nil
}

func EncodeIdentity(identity Identity) ([]byte, error) {
	if len(identity.PrivateKey) != ed25519.PrivateKeySize || len(identity.PublicKey) != ed25519.PublicKeySize ||
		subtle.ConstantTimeCompare(identity.PrivateKey[ed25519.SeedSize:], identity.PublicKey) != 1 {
		return nil, errors.New("invalid tunnel identity")
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(identity.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("encode tunnel identity: %w", err)
	}
	return encoded, nil
}

func EncodePublicKey(key ed25519.PublicKey) string {
	if len(key) != ed25519.PublicKeySize {
		return ""
	}
	return base64.RawStdEncoding.EncodeToString(key)
}

func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	decoded, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 public key")
	}
	return append(ed25519.PublicKey(nil), decoded...), nil
}

func cloneIdentity(identity Identity) Identity {
	return Identity{
		PrivateKey: append(ed25519.PrivateKey(nil), identity.PrivateKey...),
		PublicKey:  append(ed25519.PublicKey(nil), identity.PublicKey...),
	}
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
