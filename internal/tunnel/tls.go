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
	"sync"
	"time"
)

func ClientTLSConfig(identity Identity, pinnedServer ed25519.PublicKey) (*tls.Config, error) {
	return clientTLSConfig(identity, pinnedServer, time.Now)
}

func clientTLSConfig(identity Identity, pinnedServer ed25519.PublicKey, now func() time.Time) (*tls.Config, error) {
	if len(pinnedServer) != ed25519.PublicKeySize {
		return nil, errors.New("pinned Windows tunnel key is invalid")
	}
	provider, err := newRotatingCertificate(identity, "remote-docker-mac", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, now)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		NextProtos: []string{TunnelALPN}, GetClientCertificate: provider.clientCertificate,
		InsecureSkipVerify: true, // verified against the pairing-pinned Ed25519 key below
		VerifyConnection: func(state tls.ConnectionState) error {
			return verifyPeer(state, pinnedServer, x509.ExtKeyUsageServerAuth, now())
		},
	}, nil
}

func ServerTLSConfig(identity Identity, allowedClient func(ed25519.PublicKey) bool) (*tls.Config, error) {
	return serverTLSConfig(identity, allowedClient, time.Now)
}

func serverTLSConfig(identity Identity, allowedClient func(ed25519.PublicKey) bool, now func() time.Time) (*tls.Config, error) {
	if allowedClient == nil {
		return nil, errors.New("allowed Mac tunnel identity callback is required")
	}
	provider, err := newRotatingCertificate(identity, "remote-docker-windows", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, now)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		NextProtos: []string{TunnelALPN}, GetCertificate: provider.serverCertificate,
		ClientAuth: tls.RequireAnyClientCert,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) != 1 {
				return errors.New("exactly one Mac tunnel certificate is required")
			}
			key, ok := state.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
			if !ok || len(key) != ed25519.PublicKeySize || !allowedClient(append(ed25519.PublicKey(nil), key...)) {
				return errors.New("Mac tunnel identity is not trusted")
			}
			return verifyCertificateUsage(state.PeerCertificates[0], x509.ExtKeyUsageClientAuth, now())
		},
	}, nil
}

type rotatingCertificate struct {
	mu          sync.Mutex
	identity    Identity
	name        string
	usages      []x509.ExtKeyUsage
	now         func() time.Time
	certificate *tls.Certificate
	notAfter    time.Time
}

func newRotatingCertificate(identity Identity, name string, usages []x509.ExtKeyUsage, now func() time.Time) (*rotatingCertificate, error) {
	if now == nil {
		return nil, errors.New("tunnel certificate clock is required")
	}
	provider := &rotatingCertificate{
		identity: cloneIdentity(identity), name: name, usages: append([]x509.ExtKeyUsage(nil), usages...), now: now,
	}
	if _, err := provider.current(); err != nil {
		return nil, err
	}
	return provider, nil
}

func (p *rotatingCertificate) current() (*tls.Certificate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if p.certificate != nil && now.Before(p.notAfter.Add(-time.Minute)) {
		return p.certificate, nil
	}
	certificate, err := identityCertificateAt(p.identity, p.name, p.usages, now)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, errors.New("parse generated tunnel certificate")
	}
	p.certificate = &certificate
	p.notAfter = parsed.NotAfter
	return p.certificate, nil
}

func (p *rotatingCertificate) clientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return p.current()
}

func (p *rotatingCertificate) serverCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return p.current()
}

func identityCertificate(identity Identity, name string, usages []x509.ExtKeyUsage) (tls.Certificate, error) {
	return identityCertificateAt(identity, name, usages, time.Now())
}

func identityCertificateAt(identity Identity, name string, usages []x509.ExtKeyUsage, now time.Time) (tls.Certificate, error) {
	if len(identity.PrivateKey) != ed25519.PrivateKeySize || len(identity.PublicKey) != ed25519.PublicKeySize ||
		subtle.ConstantTimeCompare(identity.PrivateKey[ed25519.SeedSize:], identity.PublicKey) != 1 {
		return tls.Certificate{}, errors.New("invalid tunnel identity")
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create tunnel certificate serial: %w", err)
	}
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

func verifyPeer(state tls.ConnectionState, pinned ed25519.PublicKey, usage x509.ExtKeyUsage, now time.Time) error {
	if state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != TunnelALPN || len(state.PeerCertificates) != 1 {
		return errors.New("tunnel TLS parameters are invalid")
	}
	key, ok := state.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok || subtle.ConstantTimeCompare(key, pinned) != 1 {
		return errors.New("tunnel peer identity does not match pairing")
	}
	return verifyCertificateUsage(state.PeerCertificates[0], usage, now)
}

func verifyCertificateUsage(certificate *x509.Certificate, usage x509.ExtKeyUsage, now time.Time) error {
	if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) ||
		len(certificate.ExtKeyUsage) != 1 || certificate.ExtKeyUsage[0] != usage {
		return errors.New("tunnel peer certificate is invalid")
	}
	return nil
}
