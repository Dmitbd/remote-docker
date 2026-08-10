package discovery

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/tunnel"
)

func TestUDPAdvertisementRejectsPublicOversizedStaleInvalidAndReplay(t *testing.T) {
	publicKey, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	identity := tunnel.Identity{PrivateKey: privateKey, PublicKey: publicKey}
	nonce := make([]byte, 32)
	_, _ = rand.Read(nonce)
	advertisement, err := signedUDPAdvertisement(identity, "Windows", nonce)
	if err != nil { t.Fatal(err) }
	encoded, _ := json.Marshal(advertisement)
	private := &net.UDPAddr{IP: net.ParseIP("192.168.1.20"), Port: tunnel.TunnelPort}
	seen := make(map[string]struct{})
	if _, err := ValidateUDPAdvertisement(encoded, private, nonce, seen); err != nil { t.Fatalf("valid advertisement rejected: %v", err) }
	if _, err := ValidateUDPAdvertisement(encoded, private, nonce, seen); err == nil { t.Fatal("replay accepted") }
	if _, err := ValidateUDPAdvertisement(encoded, &net.UDPAddr{IP: net.ParseIP("203.0.113.8"), Port: 49221}, nonce, map[string]struct{}{}); err == nil { t.Fatal("public source accepted") }
	if _, err := ValidateUDPAdvertisement(make([]byte, maxUDPDatagram+1), private, nonce, map[string]struct{}{}); err == nil { t.Fatal("oversized datagram accepted") }
	stale := append([]byte(nil), nonce...); stale[0] ^= 1
	if _, err := ValidateUDPAdvertisement(encoded, private, stale, map[string]struct{}{}); err == nil { t.Fatal("stale nonce accepted") }
	advertisement.Signature[0] ^= 1; tampered, _ := json.Marshal(advertisement)
	if _, err := ValidateUDPAdvertisement(tampered, private, nonce, map[string]struct{}{}); err == nil { t.Fatal("invalid signature accepted") }
	advertisement, _ = signedUDPAdvertisement(identity, "Windows", nonce)
	advertisement.Name = "\nleaked"
	tampered, _ = json.Marshal(advertisement)
	if _, err := ValidateUDPAdvertisement(tampered, private, nonce, map[string]struct{}{}); err == nil { t.Fatal("invalid display name accepted") }
}
