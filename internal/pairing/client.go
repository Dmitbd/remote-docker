package pairing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Dmitbd/remote-docker/internal/tunnel"
)

type confirmRequest struct {
	SessionID       string `json:"session_id"`
	Code            string `json:"code"`
	ClientPublicKey []byte `json:"client_public_key"`
	DeviceID        string `json:"device_id"`
	Generation      string `json:"generation"`
	AuthorizedKey   string `json:"authorized_key"`
	RevocationProof []byte `json:"revocation_proof,omitempty"`
}

type revocationRequest struct {
	DeviceID   string `json:"device_id"`
	Generation string `json:"generation"`
	Proof      []byte `json:"proof"`
}

type sessionControlRequest struct {
	SessionID       string            `json:"session_id"`
	ClientPublicKey ed25519.PublicKey `json:"client_public_key"`
}

// Client confirms a descriptor after the user compares its visual code.
type Client struct {
	HTTPClient      *http.Client
	BaseURL         string
	Session         SessionDescriptor
	DeviceID        string
	Generation      string
	AuthorizedKey   string
	RevocationProof []byte
}

// HTTPError reports a rejected pairing request.
type HTTPError struct {
	StatusCode int
	Message    string
}

// Inspect fetches display metadata without creating a pairing session. The
// self-signed TLS key must hash to the opaque identity observed over mDNS.
// The display name remains unverified until the user confirms the OOB code.
func Inspect(ctx context.Context, baseURL, expectedInstanceID string, httpClient *http.Client) (Info, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return Info{}, errorsNewSecureURL()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+pairInfoPath, nil)
	if err != nil {
		return Info{}, fmt.Errorf("create pairing info request: %w", err)
	}
	client := httpClient
	if client == nil {
		client = unverifiedTLS13Client(5 * time.Second)
	}
	response, err := client.Do(request)
	if err != nil {
		return Info{}, fmt.Errorf("request pairing info: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (4<<10)+1))
	if err != nil {
		return Info{}, fmt.Errorf("read pairing info response: %w", err)
	}
	if len(raw) > 4<<10 {
		return Info{}, errors.New("pairing info response exceeds size limit")
	}
	if response.StatusCode != http.StatusOK {
		return Info{}, &HTTPError{StatusCode: response.StatusCode, Message: "pairing info unavailable"}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var info Info
	if err := decoder.Decode(&info); err != nil {
		return Info{}, fmt.Errorf("decode pairing info response: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Info{}, errors.New("decode pairing info response: trailing data")
	}
	if expectedInstanceID == "" {
		expectedInstanceID = info.InstanceID
	}
	if !validDisplayName(info.DisplayName) || info.InstanceID != expectedInstanceID ||
		InstanceIDFromPublicKey(info.ServerPublicKey) != expectedInstanceID || response.TLS == nil ||
		response.TLS.Version != tls.VersionTLS13 || response.TLS.NegotiatedProtocol != tunnel.PairingALPN ||
		len(response.TLS.PeerCertificates) != 1 {
		return Info{}, errors.New("pairing info discovery identity does not match TLS")
	}
	certificate := response.TLS.PeerCertificates[0]
	serverPublicKey, ok := certificate.PublicKey.(ed25519.PublicKey)
	now := time.Now()
	if !ok || now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) ||
		subtle.ConstantTimeCompare(serverPublicKey, info.ServerPublicKey) != 1 {
		return Info{}, errors.New("pairing info discovery identity does not match TLS")
	}
	return info, nil
}

// Bootstrap requests a single pairing session over private-LAN TLS. The
// returned server key is bound to the TLS certificate and then authenticated
// out of band by the six-digit comparison code.
func Bootstrap(ctx context.Context, baseURL string, clientPublicKey ed25519.PublicKey, httpClient *http.Client) (SessionDescriptor, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || len(clientPublicKey) != ed25519.PublicKeySize {
		return SessionDescriptor{}, errorsNewSecureURL()
	}
	payload, err := json.Marshal(struct {
		ClientPublicKey ed25519.PublicKey `json:"client_public_key"`
	}{ClientPublicKey: append(ed25519.PublicKey(nil), clientPublicKey...)})
	if err != nil {
		return SessionDescriptor{}, fmt.Errorf("encode pairing session request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+pairSessionPath, bytes.NewReader(payload))
	if err != nil {
		return SessionDescriptor{}, fmt.Errorf("create pairing session request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	client := httpClient
	if client == nil {
		client = unverifiedTLS13Client(15 * time.Second)
	}
	response, err := client.Do(request)
	if err != nil {
		return SessionDescriptor{}, fmt.Errorf("request pairing session: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return SessionDescriptor{}, fmt.Errorf("read pairing session response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &body)
		return SessionDescriptor{}, &HTTPError{StatusCode: response.StatusCode, Message: body.Error}
	}
	var descriptor SessionDescriptor
	if err := json.Unmarshal(raw, &descriptor); err != nil {
		return SessionDescriptor{}, fmt.Errorf("decode pairing session response: %w", err)
	}
	if !bytes.Equal(descriptor.ClientPublicKey, clientPublicKey) || response.TLS == nil ||
		response.TLS.Version != tls.VersionTLS13 || response.TLS.NegotiatedProtocol != tunnel.PairingALPN ||
		len(response.TLS.PeerCertificates) != 1 {
		return SessionDescriptor{}, ErrInvalidSession
	}
	serverPublicKey, ok := response.TLS.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok || subtle.ConstantTimeCompare(serverPublicKey, descriptor.ServerPublicKey) != 1 {
		return SessionDescriptor{}, errors.New("pairing bootstrap TLS identity does not match session")
	}
	if _, err := Code(descriptor); err != nil {
		return SessionDescriptor{}, err
	}
	return descriptor, nil
}

func unverifiedTLS13Client(timeout time.Duration) *http.Client {
	return NewDiscoveryHTTPClient(timeout, nil)
}

// NewDiscoveryHTTPClient creates the TLS 1.3 client used before the server
// identity has been authenticated by the visible pairing code.
func NewDiscoveryHTTPClient(timeout time.Duration, dialContext func(context.Context, string, string) (net.Conn, error)) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if dialContext != nil {
		transport.DialContext = dialContext
	}
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
		NextProtos: []string{tunnel.PairingALPN},
		// Trust is completed by binding the certificate key to mDNS and then
		// authenticating that key with the six-digit OOB comparison.
		InsecureSkipVerify: true, //nolint:gosec
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("pairing failed with HTTP %d: %s", e.StatusCode, e.Message)
}

// Code returns the six-digit code shown by the Mac client.
func (c *Client) Code() (string, error) {
	return Code(c.Session)
}

// Status polls the Windows decision over the TLS identity pinned during
// discovery. The visible comparison code is never submitted by the user.
func (c *Client) Status(ctx context.Context) (SessionStatus, error) {
	baseURL, err := url.Parse(c.BaseURL)
	if err != nil || baseURL.Scheme != "https" || baseURL.Host == "" || !validSessionID(c.Session.ID) {
		return SessionStatus{}, ErrInvalidSession
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + pairSessionStatusPath + "?id=" + url.QueryEscape(c.Session.ID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return SessionStatus{}, fmt.Errorf("create pairing status request: %w", err)
	}
	response, raw, err := c.doPinned(request, 16<<10)
	if err != nil {
		return SessionStatus{}, err
	}
	if response.StatusCode != http.StatusOK {
		return SessionStatus{}, decodeHTTPError(response.StatusCode, raw)
	}
	var status SessionStatus
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&status); err != nil || status.SessionID != c.Session.ID {
		return SessionStatus{}, ErrInvalidSession
	}
	return status, nil
}

// Cancel lets the initiating Mac close its own pending request. The public
// client key binds the operation to the descriptor already pinned by TLS.
func (c *Client) Cancel(ctx context.Context) error {
	baseURL, err := url.Parse(c.BaseURL)
	if err != nil || baseURL.Scheme != "https" || baseURL.Host == "" || !validSessionID(c.Session.ID) {
		return ErrInvalidSession
	}
	payload, err := json.Marshal(sessionControlRequest{
		SessionID: c.Session.ID, ClientPublicKey: append(ed25519.PublicKey(nil), c.Session.ClientPublicKey...),
	})
	if err != nil {
		return fmt.Errorf("encode pairing cancellation: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+pairSessionCancelPath, bytes.NewReader(payload),
	)
	if err != nil {
		return fmt.Errorf("create pairing cancellation request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, raw, err := c.doPinned(request, 16<<10)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return decodeHTTPError(response.StatusCode, raw)
	}
	return nil
}

// Confirm submits the public Mac identity and returns the public device record.
func (c *Client) Confirm(ctx context.Context, code string) (DeviceRecord, []byte, error) {
	baseURL, err := url.Parse(c.BaseURL)
	if err != nil || baseURL.Scheme != "https" || baseURL.Host == "" {
		return DeviceRecord{}, nil, errorsNewSecureURL()
	}

	payload, err := json.Marshal(confirmRequest{
		SessionID:       c.Session.ID,
		Code:            code,
		ClientPublicKey: append([]byte(nil), c.Session.ClientPublicKey...),
		DeviceID:        c.DeviceID,
		Generation:      c.Generation,
		AuthorizedKey:   c.AuthorizedKey,
		RevocationProof: append([]byte(nil), c.RevocationProof...),
	})
	if err != nil {
		return DeviceRecord{}, nil, fmt.Errorf("encode pairing request: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+pairPath,
		bytes.NewReader(payload),
	)
	if err != nil {
		return DeviceRecord{}, nil, fmt.Errorf("create pairing request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = pinnedHTTPClient(c.Session.ServerPublicKey)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return DeviceRecord{}, nil, fmt.Errorf("send pairing request: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return DeviceRecord{}, nil, fmt.Errorf("read pairing response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &body)
		return DeviceRecord{}, raw, &HTTPError{StatusCode: response.StatusCode, Message: body.Error}
	}

	var record DeviceRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return DeviceRecord{}, raw, fmt.Errorf("decode pairing response: %w", err)
	}
	return record, raw, nil
}

// Revoke removes one trust relationship through the pinned pairing service.
// The high-entropy proof is separate from the operational tunnel identity.
func (c *Client) Revoke(ctx context.Context, deviceID, generation string, proof []byte) error {
	baseURL, err := url.Parse(c.BaseURL)
	if err != nil || baseURL.Scheme != "https" || baseURL.Host == "" || !validDeviceID(deviceID) || !validDeviceID(generation) || len(proof) != RevocationProofSize {
		return ErrInvalidSession
	}
	payload, err := json.Marshal(revocationRequest{DeviceID: deviceID, Generation: generation, Proof: append([]byte(nil), proof...)})
	if err != nil {
		return fmt.Errorf("encode pairing revocation: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+pairRevokePath, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create pairing revocation request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, raw, err := c.doPinned(request, 16<<10)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return decodeHTTPError(response.StatusCode, raw)
	}
	return nil
}

func (c *Client) doPinned(request *http.Request, limit int64) (*http.Response, []byte, error) {
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = pinnedHTTPClient(c.Session.ServerPublicKey)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("send pairing session request: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read pairing session response: %w", err)
	}
	if int64(len(raw)) > limit {
		return nil, nil, errors.New("pairing session response exceeds size limit")
	}
	return response, raw, nil
}

func decodeHTTPError(statusCode int, raw []byte) error {
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	return &HTTPError{StatusCode: statusCode, Message: body.Error}
}

func errorsNewSecureURL() error {
	return fmt.Errorf("pairing endpoint must use a valid https URL")
}

func pinnedHTTPClient(serverPublicKey ed25519.PublicKey) *http.Client {
	return NewPinnedHTTPClient(serverPublicKey, nil)
}

// NewPinnedHTTPClient creates the TLS 1.3 client used after discovery has
// bound the self-signed certificate to its Ed25519 public key.
func NewPinnedHTTPClient(serverPublicKey ed25519.PublicKey, dialContext func(context.Context, string, string) (net.Conn, error)) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if dialContext != nil {
		transport.DialContext = dialContext
	}
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
		NextProtos: []string{tunnel.PairingALPN},
		// The certificate is intentionally self-signed and addressed by a
		// changing LAN IP. VerifyConnection pins its Ed25519 identity instead.
		InsecureSkipVerify: true, //nolint:gosec
		VerifyConnection: func(state tls.ConnectionState) error {
			if state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != tunnel.PairingALPN || len(state.PeerCertificates) != 1 {
				return fmt.Errorf("pairing server presented an invalid TLS chain")
			}
			certificate := state.PeerCertificates[0]
			now := time.Now()
			if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
				return fmt.Errorf("pairing server certificate is outside its validity window")
			}
			publicKey, ok := certificate.PublicKey.(ed25519.PublicKey)
			if !ok || subtle.ConstantTimeCompare(publicKey, serverPublicKey) != 1 {
				return fmt.Errorf("pairing server TLS identity does not match discovery")
			}
			return nil
		},
	}
	return &http.Client{Transport: transport, Timeout: 15 * time.Second}
}
