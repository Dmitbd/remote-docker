package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/Dmitbd/remote-docker/internal/credentials"
)

const SyncthingAPIKeyCredential = "syncthing-api-key"

// Client talks only to a local Syncthing REST GUI.
type Client struct {
	baseURL  *url.URL
	deviceID string
	secrets  credentials.Store
	http     *http.Client
}

// FolderStatus is the readiness subset returned by /rest/db/status.
type FolderStatus struct {
	State          string `json:"state"`
	NeedTotalItems int    `json:"needTotalItems"`
	PullErrors     int    `json:"pullErrors"`
}

// ConnectionStatus is the paired-device connectivity subset.
type ConnectionStatus struct {
	Connected bool `json:"connected"`
}

// ConnectionsResponse is returned by /rest/system/connections.
type ConnectionsResponse struct {
	Connections map[string]ConnectionStatus `json:"connections"`
}

// NewClient rejects non-loopback Syncthing REST endpoints.
func NewClient(endpoint, deviceID string, secrets credentials.Store, httpClient *http.Client) (*Client, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse Syncthing endpoint: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("Syncthing endpoint must use HTTP or HTTPS")
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("Syncthing REST endpoint must use a literal loopback address")
	}
	if parsed.Port() == "" || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Syncthing endpoint must contain only a loopback host and port")
	}
	if strings.TrimSpace(deviceID) == "" || secrets == nil || httpClient == nil {
		return nil, errors.New("Syncthing client dependencies are incomplete")
	}
	parsed.Path = ""
	localHTTP := *httpClient
	localHTTP.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{baseURL: parsed, deviceID: deviceID, secrets: secrets, http: &localHTTP}, nil
}

// ReplaceDevices applies the complete managed device set.
func (c *Client) ReplaceDevices(ctx context.Context, devices []DeviceConfig) error {
	return c.doJSON(ctx, http.MethodPut, "/rest/config/devices", nil, devices, nil)
}

// ReplaceFolders applies the complete managed folder set.
func (c *Client) ReplaceFolders(ctx context.Context, folders []FolderConfig) error {
	return c.doJSON(ctx, http.MethodPut, "/rest/config/folders", nil, folders, nil)
}

// SetIgnores replaces one folder's managed ignore patterns.
func (c *Client) SetIgnores(ctx context.Context, folderID string, ignores []string) error {
	query := url.Values{"folder": []string{folderID}}
	body := struct {
		Ignore []string `json:"ignore"`
	}{Ignore: ignores}
	return c.doJSON(ctx, http.MethodPost, "/rest/db/ignores", query, body, nil)
}

// FolderStatus reads one folder's current convergence state.
func (c *Client) FolderStatus(ctx context.Context, folderID string) (FolderStatus, error) {
	query := url.Values{"folder": []string{folderID}}
	var status FolderStatus
	err := c.doJSON(ctx, http.MethodGet, "/rest/db/status", query, nil, &status)
	return status, err
}

// Connections reads current paired-device connection states.
func (c *Client) Connections(ctx context.Context) (map[string]ConnectionStatus, error) {
	var response ConnectionsResponse
	if err := c.doJSON(ctx, http.MethodGet, "/rest/system/connections", nil, nil, &response); err != nil {
		return nil, err
	}
	return response.Connections, nil
}

func (c *Client) doJSON(ctx context.Context, method, endpointPath string, query url.Values, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode Syncthing request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	requestURL := *c.baseURL
	requestURL.Path = endpointPath
	requestURL.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), body)
	if err != nil {
		return fmt.Errorf("create Syncthing request: %w", err)
	}
	apiKey, err := c.secrets.Get(c.deviceID, SyncthingAPIKeyCredential)
	if err != nil {
		return fmt.Errorf("read Syncthing API credential: %w", err)
	}
	defer clear(apiKey)
	if len(apiKey) == 0 {
		return errors.New("Syncthing API credential is empty")
	}
	request.Header.Set("X-API-Key", string(apiKey))
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("Syncthing %s %s: %w", method, endpointPath, err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("Syncthing %s %s returned HTTP %d", method, endpointPath, response.StatusCode)
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode Syncthing %s response: %w", endpointPath, err)
	}
	return nil
}
