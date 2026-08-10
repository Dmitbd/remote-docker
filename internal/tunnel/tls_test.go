package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"net"
	"testing"
	"time"
)

func TestTunnelTLSMutuallyAuthenticatesPinnedTLS13Peers(t *testing.T) {
	serverIdentity := testIdentity(t)
	clientIdentity := testIdentity(t)
	for attempt := 0; attempt < 2; attempt++ {
		clientState, serverState, err := handshakePair(t, clientIdentity, serverIdentity, clientIdentity.PublicKey)
		if err != nil {
			t.Fatalf("fresh handshake %d error = %v", attempt, err)
		}
		if clientState.Version != tls.VersionTLS13 || serverState.Version != tls.VersionTLS13 ||
			clientState.NegotiatedProtocol != TunnelALPN || serverState.NegotiatedProtocol != TunnelALPN {
			t.Fatalf("TLS state client=%#v server=%#v", clientState, serverState)
		}
	}
}

func TestTunnelTLSRejectsWrongPinsUnknownClientsAndTLS12(t *testing.T) {
	serverIdentity := testIdentity(t)
	clientIdentity := testIdentity(t)
	wrongIdentity := testIdentity(t)
	if _, _, err := handshakePair(t, clientIdentity, serverIdentity, wrongIdentity.PublicKey); err == nil {
		t.Fatal("handshake accepted an unknown client")
	}
	clientConfig, _ := ClientTLSConfig(clientIdentity, wrongIdentity.PublicKey)
	serverConfig, _ := ServerTLSConfig(serverIdentity, func(key ed25519.PublicKey) bool {
		return subtle.ConstantTimeCompare(key, clientIdentity.PublicKey) == 1
	})
	if err := handshakeConfigs(clientConfig, serverConfig); err == nil {
		t.Fatal("handshake accepted the wrong Windows pin")
	}
	clientConfig, _ = ClientTLSConfig(clientIdentity, serverIdentity.PublicKey)
	clientConfig.MaxVersion = tls.VersionTLS12
	if err := handshakeConfigs(clientConfig, serverConfig); err == nil {
		t.Fatal("handshake negotiated TLS 1.2")
	}
}

func testIdentity(t *testing.T) Identity {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	return Identity{PrivateKey: privateKey, PublicKey: publicKey}
}

func handshakePair(t *testing.T, clientIdentity, serverIdentity Identity, allowed ed25519.PublicKey) (tls.ConnectionState, tls.ConnectionState, error) {
	t.Helper()
	clientConfig, err := ClientTLSConfig(clientIdentity, serverIdentity.PublicKey)
	if err != nil {
		return tls.ConnectionState{}, tls.ConnectionState{}, err
	}
	serverConfig, err := ServerTLSConfig(serverIdentity, func(key ed25519.PublicKey) bool {
		return subtle.ConstantTimeCompare(key, allowed) == 1
	})
	if err != nil {
		return tls.ConnectionState{}, tls.ConnectionState{}, err
	}
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	tlsClient := tls.Client(client, clientConfig)
	tlsServer := tls.Server(server, serverConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errors := make(chan error, 2)
	go func() { errors <- tlsServer.HandshakeContext(ctx) }()
	go func() { errors <- tlsClient.HandshakeContext(ctx) }()
	first, second := <-errors, <-errors
	if first != nil {
		return tls.ConnectionState{}, tls.ConnectionState{}, first
	}
	if second != nil {
		return tls.ConnectionState{}, tls.ConnectionState{}, second
	}
	return tlsClient.ConnectionState(), tlsServer.ConnectionState(), nil
}

func handshakeConfigs(clientConfig, serverConfig *tls.Config) error {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	errors := make(chan error, 2)
	go func() { errors <- tls.Server(server, serverConfig).HandshakeContext(ctx) }()
	go func() { errors <- tls.Client(client, clientConfig).HandshakeContext(ctx) }()
	first, second := <-errors, <-errors
	if first != nil {
		return first
	}
	return second
}
