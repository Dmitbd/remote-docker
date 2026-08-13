package pairing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Dmitbd/remote-docker/internal/tunnel"
)

const (
	pairPath              = "/v1/pair"
	pairSessionPath       = "/v1/pair/session"
	pairSessionStatusPath = "/v1/pair/session/status"
	pairSessionCancelPath = "/v1/pair/session/cancel"
	pairInfoPath          = "/v1/pair/info"
	pairRevokePath        = "/v1/pair/revoke"
)

// Installer applies or revokes the one managed Mac authorization on Windows.
type Installer interface {
	Install(context.Context, string, string) (DeviceInfo, error)
	Revoke(context.Context, string) error
}

type sessionState struct {
	descriptor SessionDescriptor
	state      SessionState
	attempts   int
}

type terminalSession struct {
	status     SessionStatus
	httpStatus int
}

type completedTrust struct {
	sessionID  string
	deviceID   string
	generation string
	revoked    bool
}

// Server owns one short-lived pairing session.
type Server struct {
	mu           sync.Mutex
	identity     ServerIdentity
	displayName  string
	device       DeviceInfo
	installer    Installer
	sessionGuard func(context.Context) error
	afterInstall func(context.Context, TrustedPeer) error
	revoke       func(context.Context, string, string, []byte) error
	trustRevoked func(context.Context, string, string) error
	observerID   uint64
	now          func() time.Time
	random       io.Reader
	active       *sessionState
	terminal     map[string]terminalSession
	completed    *completedTrust
}

// ServerOption changes a server dependency.
type ServerOption func(*Server)

// WithClock sets the clock used for session expiry and certificates.
func WithClock(now func() time.Time) ServerOption {
	return func(server *Server) {
		if now != nil {
			server.now = now
		}
	}
}

// WithDeviceInfo sets public identifiers returned after pairing.
func WithDeviceInfo(device DeviceInfo) ServerOption {
	return func(server *Server) {
		server.device = device
	}
}

// WithInstaller configures the Windows-side managed-key installer.
func WithInstaller(installer Installer) ServerOption {
	return func(server *Server) {
		server.installer = installer
	}
}

// WithSessionGuard rejects bootstrap before any session metadata is created.
// The Windows host uses it to enforce the one-trusted-peer product limit.
func WithSessionGuard(guard func(context.Context) error) ServerOption {
	return func(server *Server) {
		server.sessionGuard = guard
	}
}

// WithAfterInstall persists public trust metadata after the managed WSL key is
// installed. A failure revokes that exact key and keeps the session retryable.
func WithAfterInstall(afterInstall func(context.Context, TrustedPeer) error) ServerOption {
	return func(server *Server) {
		server.afterInstall = afterInstall
	}
}

// WithRevocation configures proof-authenticated cleanup independently from an
// operational SSH tunnel.
func WithRevocation(revoke func(context.Context, string, string, []byte) error) ServerOption {
	return func(server *Server) { server.revoke = revoke }
}

// WithDisplayName sets presentation metadata returned only over pairing TLS.
func WithDisplayName(displayName string) ServerOption {
	return func(server *Server) {
		server.displayName = strings.TrimSpace(displayName)
	}
}

// NewServer creates a pairing server with an ephemeral Ed25519 identity.
func NewServer(identity ServerIdentity, options ...ServerOption) (*Server, error) {
	if len(identity.PrivateKey) != ed25519.PrivateKeySize || len(identity.PublicKey()) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid server identity: %w", ErrInvalidSession)
	}
	if len(identity.publicKey) != 0 && !bytes.Equal(identity.publicKey, identity.PrivateKey.Public().(ed25519.PublicKey)) {
		return nil, fmt.Errorf("server public and private keys do not match: %w", ErrInvalidSession)
	}

	server := &Server{
		identity: identity,
		now:      time.Now,
		random:   rand.Reader,
		terminal: make(map[string]terminalSession),
	}
	for _, option := range options {
		option(server)
	}
	if server.displayName != "" && !validDisplayName(server.displayName) {
		return nil, errors.New("invalid pairing display name")
	}
	return server, nil
}

// InstanceID is the opaque mDNS identity bound to this server's TLS key.
func (s *Server) InstanceID() string {
	if s == nil {
		return ""
	}
	return InstanceIDFromPublicKey(s.identity.PublicKey())
}

// StartSession creates the server's only active pairing window.
func (s *Server) StartSession(clientPublicKey ed25519.PublicKey, ttl time.Duration) (SessionDescriptor, error) {
	if len(clientPublicKey) != ed25519.PublicKeySize || ttl <= 0 || ttl > MaxSessionTTL {
		return SessionDescriptor{}, ErrInvalidSession
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.active != nil {
		if now.Before(s.active.descriptor.ExpiresAt) {
			return SessionDescriptor{}, ErrSessionActive
		}
		s.finishLocked(SessionExpired, http.StatusGone, "")
	}

	nonce := make([]byte, 32)
	if _, err := io.ReadFull(s.random, nonce); err != nil {
		return SessionDescriptor{}, fmt.Errorf("generate pairing nonce: %w", err)
	}
	sessionIDBytes := make([]byte, 16)
	if _, err := io.ReadFull(s.random, sessionIDBytes); err != nil {
		return SessionDescriptor{}, fmt.Errorf("generate pairing session ID: %w", err)
	}
	descriptor := SessionDescriptor{
		ID:              hex.EncodeToString(sessionIDBytes),
		Nonce:           nonce,
		ServerPublicKey: s.identity.PublicKey(),
		ClientPublicKey: append(ed25519.PublicKey(nil), clientPublicKey...),
		ExpiresAt:       now.Add(ttl),
	}
	s.active = &sessionState{descriptor: descriptor, state: SessionPending}
	return cloneDescriptor(descriptor), nil
}

// ActiveSession returns the current descriptor and OOB code without secrets.
func (s *Server) ActiveSession() (SessionDescriptor, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	if s.active == nil {
		return SessionDescriptor{}, "", false
	}
	descriptor := cloneDescriptor(s.active.descriptor)
	code, err := Code(descriptor)
	if err != nil {
		return SessionDescriptor{}, "", false
	}
	return descriptor, code, true
}

// SessionStatus returns only the state of a known, unguessable session.
func (s *Server) SessionStatus(sessionID string) (SessionStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	if s.active != nil && sessionID == s.active.descriptor.ID {
		return s.activeStatusLocked(), true
	}
	terminal, ok := s.terminal[sessionID]
	return terminal.status, ok
}

// Approve records the explicit Windows-side user decision. It does not install
// credentials; installation happens only when the pinned Mac client completes.
func (s *Server) Approve(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	if s.active == nil || s.active.descriptor.ID != sessionID {
		return ErrInvalidSession
	}
	if s.active.state == SessionApproved {
		return nil
	}
	if s.active.state != SessionPending {
		return ErrSessionState
	}
	s.active.state = SessionApproved
	return nil
}

// Reject closes the request after an explicit Windows-side rejection.
func (s *Server) Reject(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	if s.active == nil || s.active.descriptor.ID != sessionID {
		return ErrInvalidSession
	}
	if s.active.state != SessionPending {
		return ErrSessionState
	}
	s.finishLocked(SessionRejected, http.StatusForbidden, "")
	return nil
}

// CancelSession closes the exact local Windows pairing session while it is
// pending or approved. Completed trust is never changed by this operation.
func (s *Server) CancelSession(sessionID string) (SessionStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	if s.active == nil || s.active.descriptor.ID != sessionID {
		if terminal, ok := s.terminal[sessionID]; ok {
			return terminal.status, ErrSessionState
		}
		return SessionStatus{}, ErrInvalidSession
	}
	if s.active.state != SessionPending && s.active.state != SessionApproved {
		return s.activeStatusLocked(), ErrSessionState
	}
	s.finishLocked(SessionCancelled, http.StatusConflict, "")
	return s.terminal[sessionID].status, nil
}

// BindTrustRevokedObserver binds the Windows lifecycle observer. The observer
// is copied under the server lock but is always invoked after the lock exits.
func (s *Server) BindTrustRevokedObserver(observer func(context.Context, string, string) error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.trustRevoked = observer
	s.observerID++
	var pending completedTrust
	if s.completed != nil {
		pending = *s.completed
	}
	s.mu.Unlock()
	if observer != nil && pending.revoked {
		_ = s.deliverTrustRevoked(context.Background(), pending)
	}
}

// PublishTrustRevoked retains and publishes the exact durable device
// generation even when the pairing session belonged to an earlier process.
func (s *Server) PublishTrustRevoked(ctx context.Context, deviceID, generation string) error {
	if s == nil || strings.TrimSpace(deviceID) == "" || strings.TrimSpace(generation) == "" {
		return nil
	}
	s.mu.Lock()
	s.expireLocked()
	var completed completedTrust
	if s.completed == nil || s.completed.revoked {
		s.completed = &completedTrust{deviceID: deviceID, generation: generation, revoked: true}
		completed = *s.completed
	} else if s.completed.deviceID == deviceID && s.completed.generation == generation {
		s.completed.revoked = true
		completed = *s.completed
	}
	s.mu.Unlock()
	if completed.deviceID == "" {
		return nil
	}
	return s.deliverTrustRevoked(ctx, completed)
}

// RetryTrustRevoked redelivers only the exact durable revoke retained after a
// lifecycle observer could not acknowledge it during a state transition.
func (s *Server) RetryTrustRevoked(ctx context.Context, deviceID, generation string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	var completed completedTrust
	if s.completed != nil && s.completed.revoked && s.completed.deviceID == deviceID && s.completed.generation == generation {
		completed = *s.completed
	}
	s.mu.Unlock()
	if completed.deviceID == "" {
		return nil
	}
	return s.deliverTrustRevoked(ctx, completed)
}

func (s *Server) deliverTrustRevoked(ctx context.Context, completed completedTrust) error {
	for attempts := 0; attempts < 2; attempts++ {
		s.mu.Lock()
		if s.completed == nil || !s.completed.revoked || s.completed.deviceID != completed.deviceID ||
			s.completed.generation != completed.generation {
			s.mu.Unlock()
			return nil
		}
		observer := s.trustRevoked
		observerID := s.observerID
		s.mu.Unlock()
		if observer == nil {
			return ErrSessionState
		}
		if err := observer(ctx, completed.deviceID, completed.generation); err != nil {
			return err
		}
		s.mu.Lock()
		if observerID != s.observerID {
			s.mu.Unlock()
			continue
		}
		if s.completed != nil && s.completed.revoked && s.completed.deviceID == completed.deviceID &&
			s.completed.generation == completed.generation {
			s.completed = nil
		}
		s.mu.Unlock()
		return nil
	}
	return ErrSessionState
}

// CompletedTrust resolves the exact durable owner recorded by the completed
// pairing session. It is internal orchestration data and is not exposed on the
// pairing wire protocol.
func (s *Server) CompletedTrust(sessionID string) (string, string, bool) {
	if s == nil {
		return "", "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.completed == nil || s.completed.sessionID != sessionID {
		return "", "", false
	}
	return s.completed.deviceID, s.completed.generation, true
}

// ServeHTTP accepts the one confirmation request that completes pairing.
func (s *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set(protocolVersionHeader, CurrentProtocolVersion)
	if request.Header.Get(protocolVersionHeader) != CurrentProtocolVersion {
		writeError(response, http.StatusUpgradeRequired, "pairing protocol upgrade required; update Remote Docker on both devices")
		return
	}
	if request.URL.Path == pairInfoPath {
		s.serveInfo(response, request)
		return
	}
	if request.URL.Path == pairSessionStatusPath {
		s.serveSessionStatus(response, request)
		return
	}
	if request.URL.Path == pairSessionCancelPath {
		s.serveSessionCancel(response, request)
		return
	}
	if request.URL.Path == pairRevokePath {
		s.serveRevocation(response, request)
		return
	}
	if request.Method == http.MethodPost && request.URL.Path == pairSessionPath {
		s.serveStartSession(response, request)
		return
	}
	if request.Method != http.MethodPost || request.URL.Path != pairPath {
		writeError(response, http.StatusNotFound, "pairing endpoint not found")
		return
	}

	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	var confirmation confirmRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&confirmation); err != nil {
		writeError(response, http.StatusBadRequest, "invalid pairing request")
		return
	}

	record, status, message := s.confirm(request.Context(), confirmation)
	if status != http.StatusOK {
		writeError(response, status, message)
		return
	}
	response.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(response).Encode(record)
}

func (s *Server) serveRevocation(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.RawQuery != "" || s.revoke == nil {
		writeError(response, http.StatusNotFound, "pairing revocation endpoint not found")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	var input revocationRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || !validDeviceID(input.DeviceID) || !validDeviceID(input.Generation) || len(input.Proof) != RevocationProofSize {
		writeError(response, http.StatusBadRequest, "invalid pairing revocation request")
		return
	}
	if err := s.revoke(request.Context(), input.DeviceID, input.Generation, append([]byte(nil), input.Proof...)); err != nil {
		writeError(response, http.StatusForbidden, "pairing revocation was not accepted")
		return
	}
	response.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(response).Encode(map[string]bool{"revoked": true})
}

func (s *Server) serveInfo(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet || request.URL.RawQuery != "" || request.ContentLength > 0 || len(request.TransferEncoding) > 0 {
		writeError(response, http.StatusBadRequest, "invalid pairing info request")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 1)
	var probe [1]byte
	if read, _ := request.Body.Read(probe[:]); read != 0 {
		writeError(response, http.StatusBadRequest, "invalid pairing info request")
		return
	}
	if !validDisplayName(s.displayName) {
		writeError(response, http.StatusServiceUnavailable, "pairing display name is unavailable")
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(response).Encode(Info{
		InstanceID: s.InstanceID(), DisplayName: s.displayName, ServerPublicKey: s.identity.PublicKey(),
	})
}

func (s *Server) serveSessionStatus(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet || request.ContentLength > 0 || len(request.TransferEncoding) > 0 {
		writeError(response, http.StatusBadRequest, "invalid pairing status request")
		return
	}
	query := request.URL.Query()
	if len(query) != 1 || len(query["id"]) != 1 || !validSessionID(query.Get("id")) {
		writeError(response, http.StatusBadRequest, "invalid pairing status request")
		return
	}
	status, ok := s.SessionStatus(query.Get("id"))
	if !ok {
		writeError(response, http.StatusNotFound, "pairing session not found")
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(response).Encode(status)
}

func (s *Server) serveSessionCancel(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.RawQuery != "" {
		writeError(response, http.StatusBadRequest, "invalid pairing cancellation request")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	var input sessionControlRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(response, http.StatusBadRequest, "invalid pairing cancellation request")
		return
	}

	s.mu.Lock()
	s.expireLocked()
	if s.active == nil || input.SessionID != s.active.descriptor.ID ||
		subtle.ConstantTimeCompare(input.ClientPublicKey, s.active.descriptor.ClientPublicKey) != 1 {
		s.mu.Unlock()
		writeError(response, http.StatusForbidden, "invalid pairing session")
		return
	}
	if s.active.state != SessionPending && s.active.state != SessionApproved {
		s.mu.Unlock()
		writeError(response, http.StatusConflict, "pairing session cannot be cancelled")
		return
	}
	s.finishLocked(SessionCancelled, http.StatusConflict, "")
	s.mu.Unlock()
	response.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(response).Encode(map[string]bool{"cancelled": true})
}

func (s *Server) serveStartSession(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	var input struct {
		ClientPublicKey ed25519.PublicKey `json:"client_public_key"`
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(response, http.StatusBadRequest, "invalid pairing session request")
		return
	}
	if s.sessionGuard != nil {
		if err := s.sessionGuard(request.Context()); err != nil {
			writeError(response, http.StatusConflict, "connection limit reached; forget the trusted device first")
			return
		}
	}
	descriptor, err := s.StartSession(input.ClientPublicKey, MaxSessionTTL)
	if errors.Is(err, ErrSessionActive) {
		writeError(response, http.StatusConflict, "a pairing session is already active")
		return
	}
	if err != nil {
		writeError(response, http.StatusBadRequest, "invalid pairing session request")
		return
	}
	response.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(response).Encode(descriptor)
}

func (s *Server) confirm(ctx context.Context, request confirmRequest) (DeviceRecord, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()

	if s.active == nil || request.SessionID != s.active.descriptor.ID {
		if terminal, ok := s.terminal[request.SessionID]; ok {
			return DeviceRecord{}, terminal.httpStatus, http.StatusText(terminal.httpStatus)
		}
		return DeviceRecord{}, http.StatusForbidden, "invalid pairing session"
	}

	session := s.active
	if session.state != SessionApproved {
		return DeviceRecord{}, http.StatusConflict, "waiting for Windows approval"
	}
	if session.attempts >= maxAttempts {
		return DeviceRecord{}, http.StatusTooManyRequests, "pairing attempt limit reached"
	}

	wantCode, err := Code(session.descriptor)
	validProof := len(request.RevocationProof) == 0 || len(request.RevocationProof) == RevocationProofSize
	valid := err == nil && validProof &&
		subtle.ConstantTimeCompare([]byte(request.Code), []byte(wantCode)) == 1 &&
		bytes.Equal(request.ClientPublicKey, session.descriptor.ClientPublicKey) &&
		validDeviceID(request.DeviceID) &&
		validDeviceID(request.Generation) &&
		validAuthorizedKey(request.AuthorizedKey)
	if !valid {
		session.attempts++
		if session.attempts == maxAttempts {
			s.finishLocked(SessionRejected, http.StatusTooManyRequests, "")
		}
		return DeviceRecord{}, http.StatusForbidden, "pairing code or device identity is invalid"
	}

	device := s.device
	if s.installer != nil {
		var installErr error
		device, installErr = s.installer.Install(ctx, request.DeviceID, request.AuthorizedKey)
		if installErr != nil || strings.TrimSpace(device.SSHHostPublicKey) == "" || strings.TrimSpace(device.SyncthingDeviceID) == "" ||
			device.SSHPort < 1 || device.SSHPort > 65535 || device.SyncthingPort < 1 || device.SyncthingPort > 65535 {
			return DeviceRecord{}, http.StatusServiceUnavailable, "managed pairing installation failed"
		}
	}
	if s.afterInstall != nil {
		peer := TrustedPeer{DeviceID: request.DeviceID, Generation: request.Generation, PublicKey: append(ed25519.PublicKey(nil), session.descriptor.ClientPublicKey...)}
		if len(request.RevocationProof) == RevocationProofSize {
			peer.RevocationProofHash = sha256.Sum256(request.RevocationProof)
		}
		if err := s.afterInstall(ctx, peer); err != nil {
			if s.installer != nil {
				_ = s.installer.Revoke(ctx, request.DeviceID)
			}
			return DeviceRecord{}, http.StatusServiceUnavailable, "cannot persist trusted device metadata"
		}
	}
	record := DeviceRecord{
		DeviceID:          request.DeviceID,
		AuthorizedKeys:    []string{request.AuthorizedKey},
		SSHHostPublicKey:  device.SSHHostPublicKey,
		SyncthingDeviceID: device.SyncthingDeviceID,
		SSHPort:           device.SSHPort,
		SyncthingPort:     device.SyncthingPort,
		TunnelPublicKey:   s.identity.PublicKey(),
		TunnelPort:        tunnel.TunnelPort,
		TransportVersion:  tunnel.CurrentTransportVersion,
	}
	s.completed = &completedTrust{
		sessionID: session.descriptor.ID, deviceID: request.DeviceID, generation: request.Generation,
	}
	s.finishLocked(SessionCompleted, http.StatusConflict, request.DeviceID)
	return record, http.StatusOK, ""
}

func (s *Server) activeStatusLocked() SessionStatus {
	return SessionStatus{
		SessionID: s.active.descriptor.ID,
		State:     s.active.state,
		ExpiresAt: s.active.descriptor.ExpiresAt,
	}
}

func (s *Server) expireLocked() {
	if s.active != nil && !s.now().Before(s.active.descriptor.ExpiresAt) {
		s.finishLocked(SessionExpired, http.StatusGone, "")
	}
	retentionDeadline := s.now().Add(-MaxSessionTTL)
	for id, terminal := range s.terminal {
		if terminal.status.ExpiresAt.Before(retentionDeadline) {
			delete(s.terminal, id)
		}
	}
}

func (s *Server) finishLocked(state SessionState, httpStatus int, deviceID string) {
	if s.active == nil {
		return
	}
	s.terminal[s.active.descriptor.ID] = terminalSession{
		status: SessionStatus{
			SessionID: s.active.descriptor.ID,
			State:     state,
			ExpiresAt: s.active.descriptor.ExpiresAt,
			DeviceID:  deviceID,
		},
		httpStatus: httpStatus,
	}
	s.active = nil
}

func (s *Server) TLSConfig() (*tls.Config, error) {
	return &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		NextProtos: []string{tunnel.PairingALPN},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return s.ephemeralCertificate()
		},
	}, nil
}

func (s *Server) ephemeralCertificate() (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(s.random, serialLimit)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "remote-docker-pairing"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(5 * time.Minute),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificateDER, err := x509.CreateCertificate(
		s.random,
		template,
		template,
		s.identity.PublicKey(),
		s.identity.PrivateKey,
	)
	if err != nil {
		return nil, fmt.Errorf("create ephemeral pairing certificate: %w", err)
	}
	certificate := &tls.Certificate{
		Certificate: [][]byte{certificateDER},
		PrivateKey:  s.identity.PrivateKey,
	}
	return certificate, nil
}

// ValidatePrivateBindAddress permits only literal private or loopback IPs.
func ValidatePrivateBindAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid pairing bind address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() || (!ip.IsPrivate() && !ip.IsLoopback()) {
		return errors.New("pairing must bind to a literal private or loopback IP")
	}
	return nil
}

// ListenPrivate opens a listener only after enforcing the private-address rule.
func ListenPrivate(network, address string) (net.Listener, error) {
	if err := ValidatePrivateBindAddress(address); err != nil {
		return nil, err
	}
	return net.Listen(network, address)
}

func validDeviceID(deviceID string) bool {
	return deviceID != "" && len(deviceID) <= 128 && strings.TrimSpace(deviceID) == deviceID
}

func validSessionID(sessionID string) bool {
	if len(sessionID) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(sessionID)
	return err == nil && len(decoded) == 16
}

func validAuthorizedKey(key string) bool {
	return strings.HasPrefix(key, "ssh-ed25519 ") &&
		len(key) <= 16<<10 &&
		!strings.ContainsAny(key, "\r\n")
}

func cloneDescriptor(descriptor SessionDescriptor) SessionDescriptor {
	descriptor.Nonce = append([]byte(nil), descriptor.Nonce...)
	descriptor.ServerPublicKey = append(ed25519.PublicKey(nil), descriptor.ServerPublicKey...)
	descriptor.ClientPublicKey = append(ed25519.PublicKey(nil), descriptor.ClientPublicKey...)
	return descriptor
}

func writeError(response http.ResponseWriter, status int, message string) {
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]string{"error": message})
}
