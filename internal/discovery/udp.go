package discovery

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/Dmitbd/remote-docker/internal/tunnel"
)

const maxUDPDatagram = 8 << 10

type UDPAdvertisement struct {
	InstanceID string `json:"instance_id"`
	Name       string `json:"name"`
	Port       int    `json:"port"`
	PublicKey  []byte `json:"public_key"`
	Nonce      []byte `json:"nonce"`
	Signature  []byte `json:"signature"`
}

type udpProbe struct {
	Nonce []byte `json:"nonce"`
}

func DiscoverUDP(ctx context.Context) ([]Peer, error) {
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	if err := connection.SetWriteBuffer(maxUDPDatagram); err != nil {
		return nil, err
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(udpProbe{Nonce: nonce})
	if _, err := connection.WriteToUDP(payload, &net.UDPAddr{IP: net.IPv4bcast, Port: tunnel.TunnelPort}); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(750 * time.Millisecond)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	_ = connection.SetReadDeadline(deadline)
	peers := make(map[string]*Peer)
	seen := make(map[string]struct{})
	buffer := make([]byte, maxUDPDatagram+1)
	for {
		count, source, readErr := connection.ReadFromUDP(buffer)
		if readErr != nil {
			if networkErr, ok := readErr.(net.Error); ok && networkErr.Timeout() {
				return sortedPeers(peers), nil
			}
			if ctx.Err() != nil {
				return sortedPeers(peers), nil
			}
			return nil, readErr
		}
		advertisement, validateErr := ValidateUDPAdvertisement(buffer[:count], source, nonce, seen)
		if validateErr != nil {
			continue
		}
		peer := Peer{InstanceID: advertisement.InstanceID, Pairing: true, Port: advertisement.Port, Addresses: []net.IP{append(net.IP(nil), source.IP...)}}
		mergePeer(peers, peer)
	}
}

func ServeUDP(ctx context.Context, identity tunnel.Identity, name string) error {
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: tunnel.TunnelPort})
	if err != nil {
		return err
	}
	defer connection.Close()
	go func() { <-ctx.Done(); _ = connection.Close() }()
	buffer := make([]byte, maxUDPDatagram+1)
	for {
		count, source, readErr := connection.ReadFromUDP(buffer)
		if readErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return readErr
		}
		if count > maxUDPDatagram || !privateSource(source) {
			continue
		}
		var probe udpProbe
		decoderErr := json.Unmarshal(buffer[:count], &probe)
		if decoderErr != nil || len(probe.Nonce) != 32 {
			continue
		}
		advertisement, err := signedUDPAdvertisement(identity, name, probe.Nonce)
		if err != nil {
			continue
		}
		encoded, _ := json.Marshal(advertisement)
		if len(encoded) <= maxUDPDatagram {
			_, _ = connection.WriteToUDP(encoded, source)
		}
	}
}

func ValidateUDPAdvertisement(data []byte, source *net.UDPAddr, expectedNonce []byte, seen map[string]struct{}) (UDPAdvertisement, error) {
	if len(data) == 0 || len(data) > maxUDPDatagram || !privateSource(source) || len(expectedNonce) != 32 {
		return UDPAdvertisement{}, errors.New("invalid UDP discovery response")
	}
	var advertisement UDPAdvertisement
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&advertisement); err != nil || decoder.Decode(&struct{}{}) != io.EOF || len(advertisement.Nonce) != 32 ||
		string(advertisement.Nonce) != string(expectedNonce) || len(advertisement.PublicKey) != ed25519.PublicKeySize ||
		advertisement.Port != tunnel.TunnelPort || validateOpaqueID(advertisement.InstanceID) != nil ||
		!validUDPName(advertisement.Name) {
		return UDPAdvertisement{}, errors.New("invalid UDP discovery advertisement")
	}
	publicKey := ed25519.PublicKey(advertisement.PublicKey)
	if UDPInstanceIDFromPublicKey(publicKey) != advertisement.InstanceID ||
		!ed25519.Verify(publicKey, udpSignedPayload(advertisement), advertisement.Signature) {
		return UDPAdvertisement{}, errors.New("invalid UDP discovery signature")
	}
	key := source.IP.String() + "/" + advertisement.InstanceID + "/" + string(advertisement.Nonce)
	if _, replayed := seen[key]; replayed {
		return UDPAdvertisement{}, errors.New("replayed UDP discovery advertisement")
	}
	seen[key] = struct{}{}
	return advertisement, nil
}

func signedUDPAdvertisement(identity tunnel.Identity, name string, nonce []byte) (UDPAdvertisement, error) {
	if len(identity.PrivateKey) != ed25519.PrivateKeySize || len(identity.PublicKey) != ed25519.PublicKeySize || len(nonce) != 32 || !validUDPName(name) {
		return UDPAdvertisement{}, errors.New("invalid UDP discovery identity")
	}
	advertisement := UDPAdvertisement{
		InstanceID: UDPInstanceIDFromPublicKey(identity.PublicKey), Name: name, Port: tunnel.TunnelPort,
		PublicKey: append([]byte(nil), identity.PublicKey...), Nonce: append([]byte(nil), nonce...),
	}
	advertisement.Signature = ed25519.Sign(identity.PrivateKey, udpSignedPayload(advertisement))
	return advertisement, nil
}

func validUDPName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return false
	}
	for _, character := range name {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func udpSignedPayload(advertisement UDPAdvertisement) []byte {
	return []byte(advertisement.InstanceID + "\x00" + advertisement.Name + "\x00" +
		strconv.Itoa(advertisement.Port) + "\x00" + string(advertisement.Nonce))
}

func privateSource(source *net.UDPAddr) bool {
	return source != nil && source.IP != nil && source.IP.IsPrivate() && !source.IP.IsLoopback() && !source.IP.IsUnspecified()
}

func UDPInstanceIDFromPublicKey(publicKey ed25519.PublicKey) string {
	if len(publicKey) != ed25519.PublicKeySize {
		return ""
	}
	digest := sha256.Sum256(publicKey)
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:16])
}
