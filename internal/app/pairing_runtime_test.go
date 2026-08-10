package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/credentials"
	"github.com/Dmitbd/remote-docker/internal/discovery"
	"github.com/Dmitbd/remote-docker/internal/pairing"
	"github.com/Dmitbd/remote-docker/internal/tunnel"
)

func TestDiscoveryPairingCandidatesResolveNameThroughTLSWithoutStartingSession(t *testing.T) {
	serverKey := make(ed25519.PublicKey, ed25519.PublicKeySize)
	inspectCalls, bootstrapCalls := 0, 0
	transport := discoveryPairingTransport{
		discover: func(context.Context) ([]discovery.Peer, error) {
			return []discovery.Peer{{
				InstanceID: "windows-one", Pairing: true, Port: 43119,
				Addresses: []net.IP{net.ParseIP("192.168.1.20")},
			}}, nil
		},
		inspect: func(_ context.Context, endpoint, instanceID string) (pairing.Info, error) {
			inspectCalls++
			if endpoint != "https://192.168.1.20:43119" || instanceID != "windows-one" {
				t.Fatalf("Inspect(%q, %q)", endpoint, instanceID)
			}
			return pairing.Info{InstanceID: instanceID, DisplayName: "Windows Workstation", ServerPublicKey: serverKey}, nil
		},
		bootstrap: func(context.Context, string, ed25519.PublicKey) (pairing.SessionDescriptor, error) {
			bootstrapCalls++
			return pairing.SessionDescriptor{}, nil
		},
	}

	targets, err := transport.Candidates(context.Background())
	if err != nil {
		t.Fatalf("Candidates() error = %v", err)
	}
	want := []pairingTarget{{
		InstanceID: "windows-one", Name: "Windows Workstation", Address: "192.168.1.20",
		PairingPort: 43119, ServerPublicKey: serverKey,
	}}
	if !reflect.DeepEqual(targets, want) || inspectCalls != 1 || bootstrapCalls != 0 {
		t.Fatalf("targets=%#v inspect=%d bootstrap=%d, want %#v/1/0", targets, inspectCalls, bootstrapCalls, want)
	}
}

func TestDiscoveryPairingBootstrapSupportsOnlyLiteralPrivateManualTarget(t *testing.T) {
	serverKey := make(ed25519.PublicKey, ed25519.PublicKeySize)
	serverKey[0] = 3
	var inspected string
	transport := discoveryPairingTransport{
		inspect: func(_ context.Context, endpoint, instanceID string) (pairing.Info, error) {
			inspected = endpoint
			if instanceID != "" {
				t.Fatalf("manual expected instance = %q, want empty self-bound identity", instanceID)
			}
			return pairing.Info{InstanceID: "manual-crypto-id", DisplayName: "Windows", ServerPublicKey: serverKey}, nil
		},
		bootstrap: func(_ context.Context, endpoint string, _ ed25519.PublicKey) (pairing.SessionDescriptor, error) {
			return pairing.SessionDescriptor{ID: "manual", ServerPublicKey: serverKey}, nil
		},
	}
	target, _, err := transport.Bootstrap(context.Background(), "192.168.1.20", make(ed25519.PublicKey, ed25519.PublicKeySize))
	if err != nil || inspected != "https://192.168.1.20:49221" || target.PairingPort != tunnel.TunnelPort || target.InstanceID != "manual-crypto-id" {
		t.Fatalf("manual Bootstrap() = %#v, inspected=%q, err=%v", target, inspected, err)
	}
	for _, selector := range []string{"example.test", "8.8.8.8", "127.0.0.1", "::1"} {
		if _, _, err := transport.Bootstrap(context.Background(), selector, make(ed25519.PublicKey, ed25519.PublicKeySize)); err == nil {
			t.Fatalf("Bootstrap(%q) accepted unsafe manual target", selector)
		}
	}
}

func TestDiscoveryPairingSavedPeerRequiresCurrentTunnelMetadata(t *testing.T) {
	store := config.Store{Path: t.TempDir() + "/config.json"}
	legacy := config.Device{Name: "Windows", Address: "192.168.1.20", SSHPort: 49222}
	if err := store.Save(config.Config{SchemaVersion: config.CurrentSchemaVersion, ActiveDevice: "windows", Devices: map[string]config.Device{"windows": legacy}}); err != nil {
		t.Fatal(err)
	}
	transport := discoveryPairingTransport{Store: store}
	if peers := transport.savedPeers(); len(peers) != 0 {
		t.Fatalf("legacy saved peers = %#v, want none", peers)
	}
	publicKey, _, _ := ed25519.GenerateKey(rand.Reader)
	legacy.TunnelPort = tunnel.TunnelPort
	legacy.TransportVersion = tunnel.CurrentTransportVersion
	legacy.TunnelPeerPublicKey = tunnel.EncodePublicKey(publicKey)
	if err := store.Save(config.Config{SchemaVersion: config.CurrentSchemaVersion, ActiveDevice: "windows", Devices: map[string]config.Device{"windows": legacy}}); err != nil {
		t.Fatal(err)
	}
	wantInstance := pairing.InstanceIDFromPublicKey(publicKey)
	if peers := transport.savedPeers(); len(peers) != 1 || peers[0].InstanceID != wantInstance ||
		peers[0].DeviceID != "windows" || peers[0].Port != tunnel.TunnelPort {
		t.Fatalf("current saved peers = %#v", peers)
	}
}

func TestDiscoveryPairingCandidatesRequirePinnedTLSForSavedAvailability(t *testing.T) {
	peer := discovery.Peer{
		InstanceID: "cryptographic-id", DeviceID: "stable-config-id", Port: tunnel.TunnelPort,
		Addresses: []net.IP{net.ParseIP("192.168.1.20")},
	}
	for _, test := range []struct {
		name      string
		verifyErr error
		want      []pairingTarget
	}{
		{name: "saved host offline", verifyErr: errors.New("dial failed"), want: []pairingTarget{}},
		{name: "saved host has pinned TLS identity", want: []pairingTarget{{
			InstanceID: "stable-config-id", Address: "192.168.1.20", PairingPort: tunnel.TunnelPort,
			TrustedAdvertisement: true,
		}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			verifyCalls := 0
			transport := discoveryPairingTransport{
				discover: func(context.Context) ([]discovery.Peer, error) { return []discovery.Peer{peer}, nil },
				verifySaved: func(context.Context, discovery.Peer) (pairingTarget, error) {
					verifyCalls++
					if test.verifyErr != nil {
						return pairingTarget{}, test.verifyErr
					}
					return test.want[0], nil
				},
			}
			targets, err := transport.Candidates(context.Background())
			if err != nil || verifyCalls != 1 || !reflect.DeepEqual(targets, test.want) {
				t.Fatalf("Candidates() = %#v, %v; verify=%d, want %#v", targets, err, verifyCalls, test.want)
			}
		})
	}
}

func TestDiscoveryPairingSavedVerificationUsesPersistedMutualTLSIdentity(t *testing.T) {
	serverIdentity := testTunnelIdentity(t)
	clientIdentity := testTunnelIdentity(t)
	store := config.Store{Path: t.TempDir() + "/config.json"}
	deviceID := "stable-config-id"
	device := config.Device{
		Name: "Windows", Address: "192.168.1.20", TunnelPort: tunnel.TunnelPort,
		TunnelPeerPublicKey: tunnel.EncodePublicKey(serverIdentity.PublicKey), TransportVersion: tunnel.CurrentTransportVersion,
	}
	if err := store.Save(config.Config{SchemaVersion: config.CurrentSchemaVersion, ActiveDevice: deviceID, Devices: map[string]config.Device{deviceID: device}}); err != nil {
		t.Fatal(err)
	}
	secrets := credentials.NewMemoryStore()
	encodedIdentity, _ := tunnel.EncodeIdentity(clientIdentity)
	if err := secrets.Put(deviceID, tunnel.IdentityCredential, encodedIdentity); err != nil {
		t.Fatal(err)
	}
	serverTLS, _ := tunnel.ServerTLSConfig(serverIdentity, func(key ed25519.PublicKey) bool {
		return reflect.DeepEqual(key, clientIdentity.PublicKey)
	})
	transport := discoveryPairingTransport{
		Store: store, Secrets: secrets,
		TunnelDialContext: func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			go func() {
				secured := tls.Server(server, serverTLS.Clone())
				_ = secured.Handshake()
				_ = secured.Close()
			}()
			return client, nil
		},
	}
	peer := discovery.Peer{
		InstanceID: pairing.InstanceIDFromPublicKey(serverIdentity.PublicKey), DeviceID: deviceID,
		Port: tunnel.TunnelPort, Addresses: []net.IP{net.ParseIP("192.168.1.20")},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	target, err := transport.verifySavedPeer(ctx, peer)
	if err != nil || target.InstanceID != deviceID || target.Address != "192.168.1.20" || !target.TrustedAdvertisement {
		t.Fatalf("verifySavedPeer() = %#v, %v", target, err)
	}
	peer.InstanceID = "different-cryptographic-id"
	if _, err := transport.verifySavedPeer(ctx, peer); err == nil {
		t.Fatal("verifySavedPeer() accepted saved key under a different cryptographic identity")
	}
}

func testTunnelIdentity(t *testing.T) tunnel.Identity {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return tunnel.Identity{PrivateKey: privateKey, PublicKey: publicKey}
}

func TestDiscoveryPairingBootstrapRequiresExplicitInstanceAndPinsInspectedTLSKey(t *testing.T) {
	serverKey := make(ed25519.PublicKey, ed25519.PublicKeySize)
	serverKey[0] = 7
	var endpoints []string
	transport := discoveryPairingTransport{
		discover: func(context.Context) ([]discovery.Peer, error) {
			return []discovery.Peer{{
				InstanceID: "windows-one", Pairing: true, Port: 43119,
				Addresses: []net.IP{net.ParseIP("10.0.0.20"), net.ParseIP("192.168.1.20")},
			}}, nil
		},
		inspect: func(_ context.Context, endpoint, instanceID string) (pairing.Info, error) {
			if instanceID != "windows-one" {
				t.Fatalf("instanceID = %q", instanceID)
			}
			return pairing.Info{InstanceID: instanceID, DisplayName: "Windows Workstation", ServerPublicKey: serverKey}, nil
		},
		bootstrap: func(_ context.Context, endpoint string, _ ed25519.PublicKey) (pairing.SessionDescriptor, error) {
			endpoints = append(endpoints, endpoint)
			return pairing.SessionDescriptor{ID: "selected", ServerPublicKey: serverKey}, nil
		},
	}

	if _, _, err := transport.Bootstrap(context.Background(), "", make(ed25519.PublicKey, ed25519.PublicKeySize)); err == nil {
		t.Fatal("Bootstrap() accepted an empty selector")
	}
	target, _, err := transport.Bootstrap(context.Background(), "windows-one", make(ed25519.PublicKey, ed25519.PublicKeySize))
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if target.Address != "10.0.0.20" || target.Name != "Windows Workstation" ||
		!reflect.DeepEqual(target.ServerPublicKey, serverKey) || !reflect.DeepEqual(endpoints, []string{"https://10.0.0.20:43119"}) {
		t.Fatalf("target=%#v endpoints=%#v, want selected TLS-bound peer", target, endpoints)
	}
}

func TestDiscoveryPairingBootstrapRejectsMultipleDistinctInstances(t *testing.T) {
	bootstrapCalls := 0
	transport := discoveryPairingTransport{
		discover: func(context.Context) ([]discovery.Peer, error) {
			return []discovery.Peer{
				{InstanceID: "windows-one", Pairing: true, Port: 43119, Addresses: []net.IP{net.ParseIP("10.0.0.20")}},
				{InstanceID: "windows-two", Pairing: true, Port: 43119, Addresses: []net.IP{net.ParseIP("192.168.1.20")}},
			}, nil
		},
		inspect: func(context.Context, string, string) (pairing.Info, error) {
			return pairing.Info{}, errors.New("should not inspect")
		},
		bootstrap: func(context.Context, string, ed25519.PublicKey) (pairing.SessionDescriptor, error) {
			bootstrapCalls++
			return pairing.SessionDescriptor{}, nil
		},
	}

	_, _, err := transport.Bootstrap(context.Background(), "missing", make(ed25519.PublicKey, ed25519.PublicKeySize))
	if err == nil || !strings.Contains(err.Error(), "matching pairing peer") {
		t.Fatalf("Bootstrap() error = %v, want missing explicit instance", err)
	}
	if bootstrapCalls != 0 {
		t.Fatalf("bootstrap calls = %d, want 0 for ambiguous instances", bootstrapCalls)
	}
}
