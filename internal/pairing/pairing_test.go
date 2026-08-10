package pairing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPairingRevokesTrustWithPinnedCleanupProof(t *testing.T) {
	var installed TrustedPeer
	var revokedDevice string
	server, err := NewServer(
		newServerIdentity(t),
		WithAfterInstall(func(_ context.Context, peer TrustedPeer) error {
			installed = peer
			return nil
		}),
		WithRevocation(func(_ context.Context, deviceID string, proof []byte) error {
			if deviceID != installed.DeviceID || sha256.Sum256(proof) != installed.RevocationProofHash {
				return errors.New("invalid revocation proof")
			}
			revokedDevice = deviceID
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	clientPublicKey, _, _ := ed25519.GenerateKey(nil)
	descriptor, err := server.StartSession(clientPublicKey, MaxSessionTTL)
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if err := server.Approve(descriptor.ID); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	httpServer := newPairingTLSTestServer(t, server)
	defer httpServer.Close()
	proof := make([]byte, RevocationProofSize)
	if _, err := rand.Read(proof); err != nil {
		t.Fatalf("generate proof: %v", err)
	}
	client := Client{
		BaseURL: httpServer.URL, Session: descriptor, DeviceID: "mac-studio",
		AuthorizedKey: "ssh-ed25519 MANAGED-MAC-KEY", RevocationProof: proof,
	}
	code, _ := client.Code()
	if _, _, err := client.Confirm(context.Background(), code); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if installed.RevocationProofHash != sha256.Sum256(proof) {
		t.Fatalf("installed revocation hash = %x", installed.RevocationProofHash)
	}
	if err := client.Revoke(context.Background(), "mac-studio", proof); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	if revokedDevice != "mac-studio" {
		t.Fatalf("revoked device = %q", revokedDevice)
	}
}

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

	_, _, err = fixture.client.Confirm(context.Background(), clientCode)
	assertHTTPStatus(t, err, http.StatusConflict)
	if err := fixture.server.Approve(fixture.descriptor.ID); err != nil {
		t.Fatalf("Approve() error = %v", err)
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

func TestPairingBootstrapOverTLSBindsClientAndServerKeys(t *testing.T) {
	identity := newServerIdentity(t)
	server, err := NewServer(identity)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	httpServer := newPairingTLSTestServer(t, server)
	defer httpServer.Close()

	clientPublicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey(client) error = %v", err)
	}
	descriptor, err := Bootstrap(context.Background(), httpServer.URL, clientPublicKey, nil)
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if !bytes.Equal(descriptor.ServerPublicKey, identity.PublicKey()) || !bytes.Equal(descriptor.ClientPublicKey, clientPublicKey) {
		t.Fatalf("descriptor keys = server %x client %x", descriptor.ServerPublicKey, descriptor.ClientPublicKey)
	}
	clientCode, err := Code(descriptor)
	if err != nil || len(clientCode) != 6 {
		t.Fatalf("Code() = %q, %v", clientCode, err)
	}
	serverDescriptor, serverCode, ok := server.ActiveSession()
	if !ok || serverDescriptor.ID != descriptor.ID || serverCode != clientCode {
		t.Fatalf("ActiveSession() = %#v, %q, %t", serverDescriptor, serverCode, ok)
	}

	_, err = Bootstrap(context.Background(), httpServer.URL, clientPublicKey, nil)
	assertHTTPStatus(t, err, http.StatusConflict)
}

func TestPairingBootstrapRejectsWhenTrustedPeerAlreadyExists(t *testing.T) {
	server, err := NewServer(newServerIdentity(t), WithSessionGuard(func(context.Context) error {
		return errors.New("trusted peer exists")
	}))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	httpServer := newPairingTLSTestServer(t, server)
	defer httpServer.Close()
	clientPublicKey, _, _ := ed25519.GenerateKey(nil)
	_, err = Bootstrap(context.Background(), httpServer.URL, clientPublicKey, nil)
	assertHTTPStatus(t, err, http.StatusConflict)
	if _, _, active := server.ActiveSession(); active {
		t.Fatal("rejected bootstrap created an active session")
	}
}

func TestPairingInfoIsReadOnlyAndBoundToEphemeralTLSIdentity(t *testing.T) {
	identity := newServerIdentity(t)
	server, err := NewServer(identity, WithDisplayName("Windows Workstation"))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	httpServer := newPairingTLSTestServer(t, server)
	defer httpServer.Close()

	instanceID := server.InstanceID()
	info, err := Inspect(context.Background(), httpServer.URL, instanceID, nil)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if info.InstanceID != instanceID || info.DisplayName != "Windows Workstation" ||
		!bytes.Equal(info.ServerPublicKey, identity.PublicKey()) {
		t.Fatalf("pairing info = %#v", info)
	}
	if _, _, active := server.ActiveSession(); active {
		t.Fatal("Inspect() created a pairing session")
	}
}

func TestPairingInfoRejectsDiscoveryIdentityThatDoesNotMatchTLS(t *testing.T) {
	identity := newServerIdentity(t)
	server, err := NewServer(identity, WithDisplayName("Windows Workstation"))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	httpServer := newPairingTLSTestServer(t, server)
	defer httpServer.Close()

	if _, err := Inspect(context.Background(), httpServer.URL, "different-opaque-instance", nil); err == nil ||
		!strings.Contains(err.Error(), "discovery identity") {
		t.Fatalf("Inspect() error = %v, want discovery identity rejection", err)
	}
}

func TestPairingHTTPClientsUseInjectedDialer(t *testing.T) {
	wantErr := errors.New("injected dialer")
	calls := 0
	dial := func(context.Context, string, string) (net.Conn, error) {
		calls++
		return nil, wantErr
	}
	clients := []*http.Client{
		NewDiscoveryHTTPClient(time.Second, dial),
		NewPinnedHTTPClient(make(ed25519.PublicKey, ed25519.PublicKeySize), dial),
	}
	for _, client := range clients {
		transport, ok := client.Transport.(*http.Transport)
		if !ok || transport.DialContext == nil {
			t.Fatalf("transport = %#v, want injected DialContext", client.Transport)
		}
		if _, err := transport.DialContext(context.Background(), "tcp", "192.168.1.68:54397"); !errors.Is(err, wantErr) {
			t.Fatalf("DialContext() error = %v", err)
		}
	}
	if calls != len(clients) {
		t.Fatalf("dial calls = %d, want %d", calls, len(clients))
	}
}

func TestPairingInfoEndpointRejectsBodiesAndNonGETMethods(t *testing.T) {
	server, err := NewServer(newServerIdentity(t), WithDisplayName("Windows Workstation"))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPost, pairInfoPath, nil),
		httptest.NewRequest(http.MethodGet, pairInfoPath, strings.NewReader("unexpected")),
	} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code == http.StatusOK {
			t.Fatalf("%s info request with body=%t succeeded", request.Method, request.Body != http.NoBody)
		}
	}
}

func TestPairingConfirmInstallsOnlyManagedClientKeyAndReturnsInstallerMetadata(t *testing.T) {
	installer := &recordingPairInstaller{device: DeviceInfo{
		SSHHostPublicKey:  "ssh-ed25519 ACTUAL-WINDOWS-HOST",
		SyncthingDeviceID: "ACTUAL-SYNCTHING-ID",
		SSHPort:           49222,
		SyncthingPort:     49220,
	}}
	identity := newServerIdentity(t)
	server, err := NewServer(identity, WithInstaller(installer))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	clientPublicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey(client) error = %v", err)
	}
	descriptor, err := server.StartSession(clientPublicKey, MaxSessionTTL)
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	httpServer := newPairingTLSTestServer(t, server)
	defer httpServer.Close()

	client := Client{
		BaseURL: httpServer.URL, Session: descriptor,
		DeviceID: "mac-device", AuthorizedKey: "ssh-ed25519 MANAGED-MAC-KEY",
	}
	code, _ := client.Code()
	if err := server.Approve(descriptor.ID); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	record, raw, err := client.Confirm(context.Background(), code)
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if installer.installs != 1 || installer.deviceID != "mac-device" || installer.authorizedKey != "ssh-ed25519 MANAGED-MAC-KEY" {
		t.Fatalf("installer calls=%d device=%q key=%q", installer.installs, installer.deviceID, installer.authorizedKey)
	}
	if record.SSHHostPublicKey != installer.device.SSHHostPublicKey || record.SyncthingDeviceID != installer.device.SyncthingDeviceID {
		t.Fatalf("record = %#v", record)
	}
	if record.SSHPort != 49222 || record.SyncthingPort != 49220 {
		t.Fatalf("record ports = %#v", record)
	}
	if record.TunnelPort != 49221 || record.TransportVersion != 1 || !bytes.Equal(record.TunnelPublicKey, identity.PublicKey()) {
		t.Fatalf("record tunnel metadata = %#v", record)
	}
	if bytes.Contains(bytes.ToLower(raw), []byte("private")) {
		t.Fatalf("pair response contains private material: %s", raw)
	}
}

func TestPairingRollsBackManagedKeyWhenPublicTrustMetadataCannotBeSaved(t *testing.T) {
	installer := &recordingPairInstaller{device: DeviceInfo{
		SSHHostPublicKey: "ssh-ed25519 HOST", SyncthingDeviceID: "SYNC", SSHPort: 49222, SyncthingPort: 49220,
	}}
	server, err := NewServer(newServerIdentity(t), WithInstaller(installer), WithAfterInstall(func(context.Context, TrustedPeer) error {
		return errors.New("disk unavailable")
	}))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	clientPublicKey, _, _ := ed25519.GenerateKey(nil)
	descriptor, err := server.StartSession(clientPublicKey, MaxSessionTTL)
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if err := server.Approve(descriptor.ID); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	httpServer := newPairingTLSTestServer(t, server)
	defer httpServer.Close()
	client := Client{BaseURL: httpServer.URL, Session: descriptor, DeviceID: "mac", AuthorizedKey: "ssh-ed25519 KEY"}
	code, _ := client.Code()
	_, _, err = client.Confirm(context.Background(), code)
	assertHTTPStatus(t, err, http.StatusServiceUnavailable)
	if installer.installs != 1 || installer.revokes != 1 || installer.deviceID != "mac" {
		t.Fatalf("installer calls installs=%d revokes=%d device=%q", installer.installs, installer.revokes, installer.deviceID)
	}
	status, ok := server.SessionStatus(descriptor.ID)
	if !ok || status.State != SessionApproved {
		t.Fatalf("session after rollback = %#v, %t", status, ok)
	}
}

type recordingPairInstaller struct {
	device        DeviceInfo
	deviceID      string
	authorizedKey string
	installs      int
	revokes       int
	err           error
}

func (i *recordingPairInstaller) Install(_ context.Context, deviceID, authorizedKey string) (DeviceInfo, error) {
	i.installs++
	i.deviceID = deviceID
	i.authorizedKey = authorizedKey
	return i.device, i.err
}

func (i *recordingPairInstaller) Revoke(_ context.Context, deviceID string) error {
	i.revokes++
	i.deviceID = deviceID
	return i.err
}

func TestPairingRejectsWrongAndExpiredCodes(t *testing.T) {
	t.Run("wrong code", func(t *testing.T) {
		fixture := newPairingFixture(t)
		if err := fixture.server.Approve(fixture.descriptor.ID); err != nil {
			t.Fatalf("Approve() error = %v", err)
		}
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
	if err := fixture.server.Approve(fixture.descriptor.ID); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}

	for attempt := 1; attempt <= 5; attempt++ {
		_, _, err := fixture.client.Confirm(context.Background(), wrongCode(t, fixture.client))
		assertHTTPStatus(t, err, http.StatusForbidden)
	}
	_, _, err := fixture.client.Confirm(context.Background(), wrongCode(t, fixture.client))
	assertHTTPStatus(t, err, http.StatusTooManyRequests)
}

func TestPairingSessionRequiresWindowsApprovalAndPublishesState(t *testing.T) {
	fixture := newPairingFixture(t)

	status, err := fixture.client.Status(context.Background())
	if err != nil || status.State != SessionPending || status.SessionID != fixture.descriptor.ID {
		t.Fatalf("Status() = %#v, %v, want pending", status, err)
	}
	if err := fixture.server.Approve(fixture.descriptor.ID); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	status, err = fixture.client.Status(context.Background())
	if err != nil || status.State != SessionApproved {
		t.Fatalf("Status() = %#v, %v, want approved", status, err)
	}
	code, _ := fixture.client.Code()
	if _, _, err := fixture.client.Confirm(context.Background(), code); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	status, err = fixture.client.Status(context.Background())
	if err != nil || status.State != SessionCompleted || status.DeviceID != "mac-studio" {
		t.Fatalf("Status() = %#v, %v, want completed mac-studio", status, err)
	}
}

func TestPairingSessionRejectCancelAndExpiryAreObservable(t *testing.T) {
	t.Run("Windows rejects", func(t *testing.T) {
		fixture := newPairingFixture(t)
		if err := fixture.server.Reject(fixture.descriptor.ID); err != nil {
			t.Fatalf("Reject() error = %v", err)
		}
		status, err := fixture.client.Status(context.Background())
		if err != nil || status.State != SessionRejected {
			t.Fatalf("Status() = %#v, %v, want rejected", status, err)
		}
	})

	t.Run("Mac cancels", func(t *testing.T) {
		fixture := newPairingFixture(t)
		if err := fixture.client.Cancel(context.Background()); err != nil {
			t.Fatalf("Cancel() error = %v", err)
		}
		status, ok := fixture.server.SessionStatus(fixture.descriptor.ID)
		if !ok || status.State != SessionCancelled {
			t.Fatalf("SessionStatus() = %#v, %t, want cancelled", status, ok)
		}
	})

	t.Run("session expires", func(t *testing.T) {
		fixture := newPairingFixture(t)
		fixture.clock.Advance(MaxSessionTTL + time.Second)
		status, err := fixture.client.Status(context.Background())
		if err != nil || status.State != SessionExpired {
			t.Fatalf("Status() = %#v, %v, want expired", status, err)
		}
	})
}

func TestPairingSessionControlRejectsUnknownOrWrongClient(t *testing.T) {
	fixture := newPairingFixture(t)
	unknown := fixture.client.Session
	unknown.ID = strings.Repeat("f", 32)
	unknownClient := *fixture.client
	unknownClient.Session = unknown
	_, err := unknownClient.Status(context.Background())
	assertHTTPStatus(t, err, http.StatusNotFound)

	otherPublicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	wrongClient := *fixture.client
	wrongClient.Session.ClientPublicKey = otherPublicKey
	if err := wrongClient.Cancel(context.Background()); err == nil {
		t.Fatal("Cancel() succeeded for a different client key")
	}
	status, ok := fixture.server.SessionStatus(fixture.descriptor.ID)
	if !ok || status.State != SessionPending {
		t.Fatalf("SessionStatus() = %#v, %t, want pending", status, ok)
	}
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
	if config.GetCertificate == nil || len(config.Certificates) != 0 {
		t.Fatalf("TLS config = %#v, want dynamically renewed ephemeral certificate", config)
	}
	tlsCertificate, err := config.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil || len(tlsCertificate.Certificate) != 1 {
		t.Fatalf("GetCertificate() = %#v, %v", tlsCertificate, err)
	}
	certificate, err := x509.ParseCertificate(tlsCertificate.Certificate[0])
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

	httpServer := newPairingTLSTestServer(t, server)
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

func newPairingTLSTestServer(t *testing.T, server *Server) *httptest.Server {
	t.Helper()
	tlsConfig, err := server.TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig() error = %v", err)
	}
	// httptest injects its own certificate when Certificates is empty, which
	// would bypass the production GetCertificate callback and break the
	// discovery-to-TLS identity binding under test.
	certificate, err := tlsConfig.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate() error = %v", err)
	}
	tlsConfig = tlsConfig.Clone()
	tlsConfig.GetCertificate = nil
	tlsConfig.Certificates = []tls.Certificate{*certificate}
	httpServer := httptest.NewUnstartedServer(server)
	httpServer.TLS = tlsConfig
	httpServer.StartTLS()
	return httpServer
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
