package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"time"
)

func ClientTLSConfig(identity Identity, pinnedServer ed25519.PublicKey) (*tls.Config, error) {
	certificate, err := identityCertificate(identity, "remote-docker-mac", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	if err != nil {
		return nil, err
	}
	if len(pinnedServer) != ed25519.PublicKeySize {
		return nil, errors.New("pinned Windows tunnel key is invalid")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		NextProtos: []string{TunnelALPN}, Certificates: []tls.Certificate{certificate},
		InsecureSkipVerify: true, // verified against the pairing-pinned Ed25519 key below
		VerifyConnection: func(state tls.ConnectionState) error {
			return verifyPeer(state, pinnedServer, x509.ExtKeyUsageServerAuth)
		},
	}, nil
}

func ServerTLSConfig(identity Identity, allowedClient func(ed25519.PublicKey) bool) (*tls.Config, error) {
	certificate, err := identityCertificate(identity, "remote-docker-windows", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	if err != nil {
		return nil, err
	}
	if allowedClient == nil {
		return nil, errors.New("allowed Mac tunnel identity callback is required")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		NextProtos: []string{TunnelALPN}, Certificates: []tls.Certificate{certificate},
		ClientAuth: tls.RequireAnyClientCert,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) != 1 {
				return errors.New("exactly one Mac tunnel certificate is required")
			}
			key, ok := state.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
			if !ok || len(key) != ed25519.PublicKeySize || !allowedClient(append(ed25519.PublicKey(nil), key...)) {
				return errors.New("Mac tunnel identity is not trusted")
			}
			return verifyCertificateUsage(state.PeerCertificates[0], x509.ExtKeyUsageClientAuth)
		},
	}, nil
}

func identityCertificate(identity Identity, name string, usages []x509.ExtKeyUsage) (tls.Certificate, error) {
	if len(identity.PrivateKey) != ed25519.PrivateKeySize || len(identity.PublicKey) != ed25519.PublicKeySize ||
		subtle.ConstantTimeCompare(identity.PrivateKey[ed25519.SeedSize:], identity.PublicKey) != 1 {
		return tls.Certificate{}, errors.New("invalid tunnel identity")
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create tunnel certificate serial: %w", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: name},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usages,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, identity.PublicKey, identity.PrivateKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create tunnel certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: append(ed25519.PrivateKey(nil), identity.PrivateKey...)}, nil
}

func verifyPeer(state tls.ConnectionState, pinned ed25519.PublicKey, usage x509.ExtKeyUsage) error {
	if state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != TunnelALPN || len(state.PeerCertificates) != 1 {
		return errors.New("tunnel TLS parameters are invalid")
	}
	key, ok := state.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok || subtle.ConstantTimeCompare(key, pinned) != 1 {
		return errors.New("tunnel peer identity does not match pairing")
	}
	return verifyCertificateUsage(state.PeerCertificates[0], usage)
}

func verifyCertificateUsage(certificate *x509.Certificate, usage x509.ExtKeyUsage) error {
	now := time.Now()
	if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) ||
		len(certificate.ExtKeyUsage) != 1 || certificate.ExtKeyUsage[0] != usage {
		return errors.New("tunnel peer certificate is invalid")
	}
	return nil
}
