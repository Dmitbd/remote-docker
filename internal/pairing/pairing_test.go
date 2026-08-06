package pairing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPairingEndToEnd(t *testing.T) {
	fixture := newPairingFixture(t)

	serverCode, err := Code(fixture.descriptor)
	if err != nil {
		t.Fatalf("server Code() error = %v", err)
	}
	clientCode, err := fixture.client.Code()
	if err != nil {
		t.Fatalf("client Code() error = %v", err)
	}
	if serverCode != clientCode || len(serverCode) != 6 {
		t.Fatalf("pairing codes server=%q client=%q, want identical six-digit values", serverCode, clientCode)
	}

	record, raw, err := fixture.client.Confirm(context.Background(), clientCode)
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if record.DeviceID != "mac-studio" {
		t.Fatalf("DeviceID = %q, want mac-studio", record.DeviceID)
	}
	if len(record.AuthorizedKeys) != 1 || record.AuthorizedKeys[0] != fixture.authorizedKey {
		t.Fatalf("AuthorizedKeys = %#v, want one submitted key", record.AuthorizedKeys)
	}
	if record.SSHHostPublicKey != "ssh-ed25519 WINDOWS-HOST" {
		t.Fatalf("SSHHostPublicKey = %q", record.SSHHostPublicKey)
	}
	if record.SyncthingDeviceID != "WINDOWS-SYNCTHING-ID" {
		t.Fatalf("SyncthingDeviceID = %q", record.SyncthingDeviceID)
	}
	encodedPrivateKey := base64.StdEncoding.EncodeToString(fixture.serverIdentity.PrivateKey)
	if bytes.Contains(bytes.ToLower(raw), []byte("private")) ||
		bytes.Contains(raw, fixture.serverIdentity.PrivateKey) ||
		bytes.Contains(raw, []byte(encodedPrivateKey)) {
		t.Fatalf("pair response contains private key material: %s", raw)
	}

	_, _, err = fixture.client.Confirm(context.Background(), clientCode)
	assertHTTPStatus(t, err, http.StatusConflict)
}

func TestPairingRejectsWrongAndExpiredCodes(t *testing.T) {
	t.Run("wrong code", func(t *testing.T) {
		fixture := newPairingFixture(t)
		_, _, err := fixture.client.Confirm(context.Background(), wrongCode(t, fixture.client))
		assertHTTPStatus(t, err, http.StatusForbidden)
	})

	t.Run("expired code", func(t *testing.T) {
		fixture := newPairingFixture(t)
		code, err := fixture.client.Code()
		if err != nil {
			t.Fatalf("Code() error = %v", err)
		}
		fixture.clock.Advance(121 * time.Second)

		_, _, err = fixture.client.Confirm(context.Background(), code)
		assertHTTPStatus(t, err, http.StatusGone)
	})
}

func TestPairingLimitsAbuse(t *testing.T) {
	fixture := newPairingFixture(t)

	if _, err := fixture.server.StartSession(fixture.clientPublicKey, 120*time.Second); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("StartSession() error = %v, want ErrSessionActive", err)
	}

	for attempt := 1; attempt <= 5; attempt++ {
		_, _, err := fixture.client.Confirm(context.Background(), wrongCode(t, fixture.client))
		assertHTTPStatus(t, err, http.StatusForbidden)
	}
	_, _, err := fixture.client.Confirm(context.Background(), wrongCode(t, fixture.client))
	assertHTTPStatus(t, err, http.StatusTooManyRequests)
}

func TestTLSConfigUsesEphemeralEd25519IdentityAndTLS13(t *testing.T) {
	identity := newServerIdentity(t)
	server, err := NewServer(identity)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	config, err := server.TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig() error = %v", err)
	}
	if config.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %d, want TLS 1.3", config.MinVersion)
	}
	if len(config.Certificates) != 1 || len(config.Certificates[0].Certificate) != 1 {
		t.Fatalf("TLS certificates = %#v, want one ephemeral certificate", config.Certificates)
	}
	certificate, err := x509.ParseCertificate(config.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate() error = %v", err)
	}
	publicKey, ok := certificate.PublicKey.(ed25519.PublicKey)
	if !ok || !bytes.Equal(publicKey, identity.PublicKey()) {
		t.Fatal("TLS certificate does not use the server pairing identity")
	}
}

func TestClientRejectsTLSIdentityThatDoesNotMatchDiscovery(t *testing.T) {
	fixture := newPairingFixture(t)
	otherPublicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey(other server) error = %v", err)
	}
	fixture.client.Session.ServerPublicKey = otherPublicKey
	code, err := fixture.client.Code()
	if err != nil {
		t.Fatalf("Code() error = %v", err)
	}

	_, _, err = fixture.client.Confirm(context.Background(), code)
	if err == nil || !strings.Contains(err.Error(), "TLS identity does not match discovery") {
		t.Fatalf("Confirm() error = %v, want pinned TLS identity rejection", err)
	}
}

func TestValidatePrivateBindAddress(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "192.168.1.20:43119", "[fd00::20]:43119"} {
		if err := ValidatePrivateBindAddress(address); err != nil {
			t.Fatalf("ValidatePrivateBindAddress(%q) error = %v", address, err)
		}
	}
	for _, address := range []string{"0.0.0.0:43119", "[::]:43119", "8.8.8.8:43119", ":43119", "localhost:43119"} {
		if err := ValidatePrivateBindAddress(address); err == nil {
			t.Fatalf("ValidatePrivateBindAddress(%q) succeeded, want rejection", address)
		}
	}
}

type pairingFixture struct {
	server          *Server
	serverIdentity  ServerIdentity
	descriptor      SessionDescriptor
	client          *Client
	clientPublicKey ed25519.PublicKey
	authorizedKey   string
	clock           *testClock
	httpServer      *httptest.Server
}

func newPairingFixture(t *testing.T) pairingFixture {
	t.Helper()
	clock := &testClock{now: time.Now().UTC()}
	identity := newServerIdentity(t)
	server, err := NewServer(identity,
		WithClock(clock.Now),
		WithDeviceInfo(DeviceInfo{
			SSHHostPublicKey:  "ssh-ed25519 WINDOWS-HOST",
			SyncthingDeviceID: "WINDOWS-SYNCTHING-ID",
		}),
	)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	clientPublicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey(client) error = %v", err)
	}
	descriptor, err := server.StartSession(clientPublicKey, 120*time.Second)
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	tlsConfig, err := server.TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig() error = %v", err)
	}
	httpServer := httptest.NewUnstartedServer(server)
	httpServer.TLS = tlsConfig
	httpServer.StartTLS()
	t.Cleanup(httpServer.Close)

	authorizedKey := "ssh-ed25519 MAC-PAIR-KEY"
	client := &Client{
		BaseURL:       httpServer.URL,
		Session:       descriptor,
		DeviceID:      "mac-studio",
		AuthorizedKey: authorizedKey,
	}

	return pairingFixture{
		server:          server,
		serverIdentity:  identity,
		descriptor:      descriptor,
		client:          client,
		clientPublicKey: clientPublicKey,
		authorizedKey:   authorizedKey,
		clock:           clock,
		httpServer:      httpServer,
	}
}

func newServerIdentity(t *testing.T) ServerIdentity {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey(server) error = %v", err)
	}
	return ServerIdentity{PrivateKey: privateKey, publicKey: publicKey}
}

func assertHTTPStatus(t *testing.T, err error, want int) {
	t.Helper()
	var statusError *HTTPError
	if !errors.As(err, &statusError) {
		t.Fatalf("error = %v, want HTTPError status %d", err, want)
	}
	if statusError.StatusCode != want {
		t.Fatalf("HTTP status = %d, want %d", statusError.StatusCode, want)
	}
}

func wrongCode(t *testing.T, client *Client) string {
	t.Helper()
	code, err := client.Code()
	if err != nil {
		t.Fatalf("Code() error = %v", err)
	}
	if code[0] == '9' {
		return "8" + code[1:]
	}
	return "9" + code[1:]
}

type testClock struct {
	now time.Time
}

func (c *testClock) Now() time.Time {
	return c.now
}

func (c *testClock) Advance(duration time.Duration) {
	c.now = c.now.Add(duration)
}

func TestPairingResponseJSONHasNoUnexportedIdentity(t *testing.T) {
	record := DeviceRecord{DeviceID: "mac", AuthorizedKeys: []string{"ssh-ed25519 public"}}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(strings.ToLower(string(encoded)), "private") {
		t.Fatalf("record JSON contains private field: %s", encoded)
	}
}
