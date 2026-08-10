package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/credentials"
)

func TestLoadOrCreateIdentityCreatesLoadsAndDefensivelyCopies(t *testing.T) {
	store := credentials.NewMemoryStore()
	created, err := LoadOrCreateIdentity(store, WindowsIdentityOwner)
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity(create) error = %v", err)
	}
	wantPublic := append([]byte(nil), created.PublicKey...)
	created.PublicKey[0] ^= 0xff
	created.PrivateKey[0] ^= 0xff
	loaded, err := LoadOrCreateIdentity(store, WindowsIdentityOwner)
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity(load) error = %v", err)
	}
	if string(loaded.PublicKey) != string(wantPublic) {
		t.Fatalf("loaded public key changed: %x want %x", loaded.PublicKey, wantPublic)
	}
	loaded.PublicKey[0] ^= 0xff
	again, err := LoadOrCreateIdentity(store, WindowsIdentityOwner)
	if err != nil || string(again.PublicKey) != string(wantPublic) {
		t.Fatalf("stored identity was aliased: public=%x error=%v", again.PublicKey, err)
	}
}

func TestIdentityRejectsCorruptLengthAndMismatchedPublicKey(t *testing.T) {
	if _, err := IdentityFromPKCS8([]byte("short")); err == nil {
		t.Fatal("IdentityFromPKCS8 accepted corrupt input")
	}
	publicKey, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	otherPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := EncodeIdentity(Identity{PrivateKey: privateKey, PublicKey: otherPublic}); err == nil {
		t.Fatal("EncodeIdentity accepted mismatched public key")
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(privateKey[:ed25519.SeedSize])
	if err == nil {
		if _, parseErr := IdentityFromPKCS8(encoded); parseErr == nil {
			t.Fatal("IdentityFromPKCS8 accepted invalid private-key length")
		}
	}
	if decoded, err := ParsePublicKey(EncodePublicKey(publicKey)); err != nil || string(decoded) != string(publicKey) {
		t.Fatalf("public-key round trip = %x error=%v", decoded, err)
	}
}
