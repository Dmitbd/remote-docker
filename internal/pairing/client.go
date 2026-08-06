package pairing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type confirmRequest struct {
	SessionID       string `json:"session_id"`
	Code            string `json:"code"`
	ClientPublicKey []byte `json:"client_public_key"`
	DeviceID        string `json:"device_id"`
	AuthorizedKey   string `json:"authorized_key"`
}

// Client confirms a descriptor after the user compares its visual code.
type Client struct {
	HTTPClient    *http.Client
	BaseURL       string
	Session       SessionDescriptor
	DeviceID      string
	AuthorizedKey string
}

// HTTPError reports a rejected pairing request.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("pairing failed with HTTP %d: %s", e.StatusCode, e.Message)
}

// Code returns the six-digit code shown by the Mac client.
func (c *Client) Code() (string, error) {
	return Code(c.Session)
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
		AuthorizedKey:   c.AuthorizedKey,
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

func errorsNewSecureURL() error {
	return fmt.Errorf("pairing endpoint must use a valid https URL")
}

func pinnedHTTPClient(serverPublicKey ed25519.PublicKey) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The certificate is intentionally self-signed and addressed by a
		// changing LAN IP. VerifyConnection pins its Ed25519 identity instead.
		InsecureSkipVerify: true, //nolint:gosec
		VerifyConnection: func(state tls.ConnectionState) error {
			if state.Version != tls.VersionTLS13 || len(state.PeerCertificates) != 1 {
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
