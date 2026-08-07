package pairing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
)

const (
	pairPath        = "/v1/pair"
	pairSessionPath = "/v1/pair/session"
	pairInfoPath    = "/v1/pair/info"
)

// Installer applies or revokes the one managed Mac authorization on Windows.
type Installer interface {
	Install(context.Context, string, string) (DeviceInfo, error)
	Revoke(context.Context, string) error
}

type sessionState struct {
	descriptor SessionDescriptor
	attempts   int
}

// Server owns one short-lived pairing session.
type Server struct {
	mu          sync.Mutex
	identity    ServerIdentity
	displayName string
	device      DeviceInfo
	installer   Installer
	now         func() time.Time
	random      io.Reader
	active      *sessionState
	terminal    map[string]int
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
		terminal: make(map[string]int),
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
		s.terminal[s.active.descriptor.ID] = http.StatusGone
		s.active = nil
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
	s.active = &sessionState{descriptor: descriptor}
	return cloneDescriptor(descriptor), nil
}

// ActiveSession returns the current descriptor and OOB code without secrets.
func (s *Server) ActiveSession() (SessionDescriptor, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil || !s.now().Before(s.active.descriptor.ExpiresAt) {
		return SessionDescriptor{}, "", false
	}
	descriptor := cloneDescriptor(s.active.descriptor)
	code, err := Code(descriptor)
	if err != nil {
		return SessionDescriptor{}, "", false
	}
	return descriptor, code, true
}

// ServeHTTP accepts the one confirmation request that completes pairing.
func (s *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "application/json")
	if request.URL.Path == pairInfoPath {
		s.serveInfo(response, request)
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

	if s.active == nil || request.SessionID != s.active.descriptor.ID {
		if status, ok := s.terminal[request.SessionID]; ok {
			return DeviceRecord{}, status, http.StatusText(status)
		}
		return DeviceRecord{}, http.StatusForbidden, "invalid pairing session"
	}

	session := s.active
	if !s.now().Before(session.descriptor.ExpiresAt) {
		s.terminal[request.SessionID] = http.StatusGone
		s.active = nil
		return DeviceRecord{}, http.StatusGone, "pairing session expired"
	}
	if session.attempts >= maxAttempts {
		return DeviceRecord{}, http.StatusTooManyRequests, "pairing attempt limit reached"
	}

	wantCode, err := Code(session.descriptor)
	valid := err == nil &&
		subtle.ConstantTimeCompare([]byte(request.Code), []byte(wantCode)) == 1 &&
		bytes.Equal(request.ClientPublicKey, session.descriptor.ClientPublicKey) &&
		validDeviceID(request.DeviceID) &&
		validAuthorizedKey(request.AuthorizedKey)
	if !valid {
		session.attempts++
		if session.attempts == maxAttempts {
			s.terminal[request.SessionID] = http.StatusTooManyRequests
			s.active = nil
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
	record := DeviceRecord{
		DeviceID:          request.DeviceID,
		AuthorizedKeys:    []string{request.AuthorizedKey},
		SSHHostPublicKey:  device.SSHHostPublicKey,
		SyncthingDeviceID: device.SyncthingDeviceID,
		SSHPort:           device.SSHPort,
		SyncthingPort:     device.SyncthingPort,
	}
	s.terminal[request.SessionID] = http.StatusConflict
	s.active = nil
	return record, http.StatusOK, ""
}

func (s *Server) TLSConfig() (*tls.Config, error) {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
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
