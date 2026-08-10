package tunnel

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"
)

func TestRouteTLSSeparatesPairingAndTunnelAndClosesBoth(t *testing.T) {
	identity := testIdentity(t)
	certificate, err := identityCertificate(identity, "router", nil)
	if err != nil {
		t.Fatalf("identityCertificate() error = %v", err)
	}
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	routed, err := RouteTLS(ctx, base, &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate}, NextProtos: []string{PairingALPN, TunnelALPN},
	})
	if err != nil {
		t.Fatalf("RouteTLS() error = %v", err)
	}

	pairingClient := dialRoutedTLS(t, base.Addr().String(), PairingALPN)
	tunnelClient := dialRoutedTLS(t, base.Addr().String(), TunnelALPN)
	pairingConnection, err := routed.Pairing.Accept()
	if err != nil {
		t.Fatalf("Pairing.Accept() error = %v", err)
	}
	tunnelConnection, err := routed.Tunnel.Accept()
	if err != nil {
		t.Fatalf("Tunnel.Accept() error = %v", err)
	}
	if pairingConnection.(*tls.Conn).ConnectionState().NegotiatedProtocol != PairingALPN ||
		tunnelConnection.(*tls.Conn).ConnectionState().NegotiatedProtocol != TunnelALPN {
		t.Fatal("connections were routed to the wrong listeners")
	}
	_ = pairingClient.Close()
	_ = tunnelClient.Close()
	_ = pairingConnection.Close()
	_ = tunnelConnection.Close()
	cancel()
	assertAcceptClosed(t, routed.Pairing)
	assertAcceptClosed(t, routed.Tunnel)
}

func TestRouteTLSRejectsUnsupportedALPNAndShutdownDuringHandshake(t *testing.T) {
	identity := testIdentity(t)
	certificate, _ := identityCertificate(identity, "router", nil)
	base, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	routed, _ := RouteTLS(ctx, base, &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate}, NextProtos: []string{PairingALPN, TunnelALPN},
	})
	unsupported, err := tls.Dial("tcp", base.Addr().String(), &tls.Config{
		MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: []string{"h2"},
	})
	if err == nil {
		_ = unsupported.Close()
	}
	slow, err := net.Dial("tcp", base.Addr().String())
	if err != nil {
		t.Fatalf("Dial(slow) error = %v", err)
	}
	cancel()
	_ = slow.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 1)
	if _, err := slow.Read(buffer); err == nil {
		t.Fatal("slow handshake remained open after shutdown")
	}
	_ = slow.Close()
	assertAcceptClosed(t, routed.Pairing)
	assertAcceptClosed(t, routed.Tunnel)
}

func dialRoutedTLS(t *testing.T, address, protocol string) *tls.Conn {
	t.Helper()
	connection, err := tls.Dial("tcp", address, &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		InsecureSkipVerify: true, NextProtos: []string{protocol},
	})
	if err != nil {
		t.Fatalf("tls.Dial(%s) error = %v", protocol, err)
	}
	return connection
}

func assertAcceptClosed(t *testing.T, listener net.Listener) {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, err := listener.Accept(); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Accept() succeeded after close")
		}
	case <-time.After(time.Second):
		t.Fatal("Accept() did not unblock after close")
	}
}
