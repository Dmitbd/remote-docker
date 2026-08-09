package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Dmitbd/remote-docker/internal/config"
	"github.com/Dmitbd/remote-docker/internal/credentials"
	"github.com/Dmitbd/remote-docker/internal/discovery"
	"github.com/Dmitbd/remote-docker/internal/dockercli"
	"github.com/Dmitbd/remote-docker/internal/localapi"
	"github.com/Dmitbd/remote-docker/internal/pairing"
	"github.com/Dmitbd/remote-docker/internal/sshtransport"
	"github.com/Dmitbd/remote-docker/internal/systemtransport"
	"golang.org/x/crypto/ssh"
)

const pairingDiscoveryTimeout = 3 * time.Second

type pairingTarget struct {
	InstanceID      string
	Name            string
	Address         string
	PairingPort     int
	ServerPublicKey ed25519.PublicKey
}

type pairingTransport interface {
	Candidates(context.Context) ([]pairingTarget, error)
	Bootstrap(context.Context, string, ed25519.PublicKey) (pairingTarget, pairing.SessionDescriptor, error)
	Status(context.Context, pairingTarget, pairing.SessionDescriptor) (pairing.SessionStatus, error)
	Cancel(context.Context, pairingTarget, pairing.SessionDescriptor) error
	Confirm(context.Context, pairingTarget, pairing.SessionDescriptor, string, string, string) (pairing.DeviceRecord, error)
	Revoke(context.Context, config.Device, string) error
}

type macPairingOptions struct {
	Store           config.Store
	Secrets         credentials.Store
	Transport       pairingTransport
	Docker          dockercli.Executor
	DockerCLI       string
	DockerContext   string
	SSHConfigPath   string
	KnownHostsPath  string
	AgentSocketPath string
	ControlDir      string
	ClientDeviceID  func(context.Context) (string, error)
}

type pendingPairing struct {
	target         pairingTarget
	descriptor     pairing.SessionDescriptor
	clientDeviceID string
	authorizedKey  string
	privateKeyPEM  []byte
	completing     bool
	record         *pairing.DeviceRecord
}

type macPairingCoordinator struct {
	options macPairingOptions
	mu      sync.Mutex
	pending *pendingPairing
}

func newMacPairingCoordinator(options macPairingOptions) *macPairingCoordinator {
	return &macPairingCoordinator{options: options}
}

func (c *macPairingCoordinator) Candidates(ctx context.Context) (localapi.PairCandidatesResult, error) {
	if c == nil || c.options.Transport == nil {
		return localapi.PairCandidatesResult{}, unavailable("pairing discovery is unavailable")
	}
	targets, err := c.options.Transport.Candidates(ctx)
	if err != nil {
		return localapi.PairCandidatesResult{}, unavailable("cannot discover pairing devices")
	}
	result := localapi.PairCandidatesResult{Candidates: make([]localapi.PairingCandidate, 0, len(targets))}
	for _, target := range targets {
		if strings.TrimSpace(target.InstanceID) == "" || strings.TrimSpace(target.Name) == "" {
			continue
		}
		result.Candidates = append(result.Candidates, localapi.PairingCandidate{
			ID: target.InstanceID, Name: target.Name, Unverified: true,
		})
	}
	sort.Slice(result.Candidates, func(i, j int) bool {
		if result.Candidates[i].Name == result.Candidates[j].Name {
			return result.Candidates[i].ID < result.Candidates[j].ID
		}
		return result.Candidates[i].Name < result.Candidates[j].Name
	})
	return result, nil
}

func (c *macPairingCoordinator) Start(ctx context.Context, selector string) (localapi.PairStartResult, error) {
	if c == nil || c.options.Secrets == nil || c.options.Transport == nil || c.options.Docker == nil {
		return localapi.PairStartResult{}, unavailable("pairing infrastructure is unavailable")
	}
	c.mu.Lock()
	if c.pending != nil && time.Now().Before(c.pending.descriptor.ExpiresAt) {
		c.mu.Unlock()
		return localapi.PairStartResult{}, needsAction("a pairing session is already active")
	}
	if c.pending != nil {
		clearSecret(c.pending.privateKeyPEM)
		c.pending = nil
	}
	c.mu.Unlock()

	clientPublicKey, clientPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return localapi.PairStartResult{}, unavailable("cannot create pairing identity")
	}
	sshPublicKey, err := ssh.NewPublicKey(clientPublicKey)
	if err != nil {
		clearSecret(clientPrivateKey)
		return localapi.PairStartResult{}, unavailable("cannot create SSH pairing identity")
	}
	privateBlock, err := ssh.MarshalPrivateKey(clientPrivateKey, "remote-docker managed key")
	clearSecret(clientPrivateKey)
	if err != nil {
		return localapi.PairStartResult{}, unavailable("cannot encode SSH pairing identity")
	}
	privateKeyPEM := pem.EncodeToMemory(privateBlock)
	clientDeviceIDProvider := c.options.ClientDeviceID
	if clientDeviceIDProvider == nil {
		clientDeviceIDProvider = func(context.Context) (string, error) { return randomDeviceID() }
	}
	clientDeviceID, err := clientDeviceIDProvider(ctx)
	if err != nil || strings.TrimSpace(clientDeviceID) == "" {
		clearSecret(privateKeyPEM)
		return localapi.PairStartResult{}, unavailable("cannot create client device identity")
	}
	target, descriptor, err := c.options.Transport.Bootstrap(ctx, selector, clientPublicKey)
	if err != nil {
		clearSecret(privateKeyPEM)
		return localapi.PairStartResult{}, unavailable("cannot start private-LAN pairing session")
	}
	code, err := pairing.Code(descriptor)
	if err != nil {
		clearSecret(privateKeyPEM)
		return localapi.PairStartResult{}, unavailable("pairing session is invalid")
	}
	pending := &pendingPairing{
		target: target, descriptor: descriptor, clientDeviceID: clientDeviceID,
		authorizedKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublicKey))),
		privateKeyPEM: privateKeyPEM,
	}
	c.mu.Lock()
	c.pending = pending
	c.mu.Unlock()
	return localapi.PairStartResult{
		SessionID: descriptor.ID, Code: code,
		Peer:      localapi.LifecyclePeer{ID: target.InstanceID, Name: target.Name, OS: "windows", Address: target.Address},
		ExpiresAt: descriptor.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}, nil
}

func (c *macPairingCoordinator) Status(ctx context.Context, sessionID string) (localapi.PairingStatusResult, error) {
	if c == nil {
		return localapi.PairingStatusResult{}, unavailable("pairing infrastructure is unavailable")
	}
	c.mu.Lock()
	if sessionID == "" && c.pending != nil {
		sessionID = c.pending.descriptor.ID
	}
	if c.pending == nil || c.pending.descriptor.ID != sessionID {
		c.mu.Unlock()
		return localapi.PairingStatusResult{}, needsAction("pairing session is not active")
	}
	pending := *c.pending
	pending.privateKeyPEM = append([]byte(nil), c.pending.privateKeyPEM...)
	if c.pending.record != nil {
		record := *c.pending.record
		record.AuthorizedKeys = append([]string(nil), c.pending.record.AuthorizedKeys...)
		pending.record = &record
	}
	c.mu.Unlock()
	status, err := c.options.Transport.Status(ctx, pending.target, pending.descriptor)
	if err != nil {
		clearSecret(pending.privateKeyPEM)
		return localapi.PairingStatusResult{}, unavailable("cannot read the Windows pairing decision")
	}
	result := pairingStatusResult(pending, status)
	if status.State == pairing.SessionRejected || status.State == pairing.SessionCancelled || status.State == pairing.SessionExpired {
		c.clearPending(sessionID)
		clearSecret(pending.privateKeyPEM)
		return result, nil
	}
	if status.State != pairing.SessionApproved && !(status.State == pairing.SessionCompleted && pending.record != nil) {
		clearSecret(pending.privateKeyPEM)
		return result, nil
	}
	c.mu.Lock()
	if c.pending == nil || c.pending.descriptor.ID != sessionID {
		c.mu.Unlock()
		clearSecret(pending.privateKeyPEM)
		return localapi.PairingStatusResult{}, needsAction("pairing session is not active")
	}
	if c.pending.completing {
		c.mu.Unlock()
		clearSecret(pending.privateKeyPEM)
		return result, nil
	}
	c.pending.completing = true
	c.mu.Unlock()
	device, err := c.complete(ctx, sessionID, pending)
	if err != nil {
		c.mu.Lock()
		if c.pending != nil && c.pending.descriptor.ID == sessionID {
			c.pending.completing = false
		}
		c.mu.Unlock()
		clearSecret(pending.privateKeyPEM)
		return localapi.PairingStatusResult{}, err
	}
	result.Status = string(pairing.SessionCompleted)
	result.Device = &device
	return result, nil
}

func (c *macPairingCoordinator) complete(ctx context.Context, sessionID string, pending pendingPairing) (localapi.Device, error) {
	var record pairing.DeviceRecord
	if pending.record == nil {
		wantCode, err := pairing.Code(pending.descriptor)
		if err != nil {
			return localapi.Device{}, unavailable("pairing session is invalid")
		}
		record, err = c.options.Transport.Confirm(
			ctx, pending.target, pending.descriptor, pending.clientDeviceID, pending.authorizedKey, wantCode,
		)
		if err != nil {
			return localapi.Device{}, unavailable("pairing confirmation failed")
		}
		c.mu.Lock()
		if c.pending != nil && c.pending.descriptor.ID == sessionID {
			stored := record
			stored.AuthorizedKeys = append([]string(nil), record.AuthorizedKeys...)
			c.pending.record = &stored
		}
		c.mu.Unlock()
	} else {
		record = *pending.record
		record.AuthorizedKeys = append([]string(nil), pending.record.AuthorizedKeys...)
	}
	remoteDeviceID, err := pairedRemoteDeviceID(record.SSHHostPublicKey)
	if err != nil || strings.TrimSpace(record.SyncthingDeviceID) == "" ||
		record.SSHPort < 1 || record.SSHPort > 65535 || record.SyncthingPort < 1 || record.SyncthingPort > 65535 {
		clearSecret(pending.privateKeyPEM)
		return localapi.Device{}, unavailable("paired device returned invalid public metadata")
	}
	alias := "remote-docker-device-" + remoteDeviceID
	if err := sshtransport.PinKnownHost(c.options.KnownHostsPath, alias, record.SSHHostPublicKey); err != nil {
		clearSecret(pending.privateKeyPEM)
		return localapi.Device{}, unavailable("cannot pin paired SSH identity")
	}
	if err := sshtransport.WriteConfig(c.options.SSHConfigPath, sshtransport.Config{
		DeviceID: remoteDeviceID, HostName: pending.target.Address, Port: record.SSHPort,
		AgentSocket: c.options.AgentSocketPath, KnownHostsFile: c.options.KnownHostsPath, ControlDir: c.options.ControlDir,
	}); err != nil {
		clearSecret(pending.privateKeyPEM)
		return localapi.Device{}, unavailable("cannot write managed SSH configuration")
	}
	if err := dockercli.EnsureContext(ctx, c.options.Docker, c.options.DockerCLI, c.options.DockerContext, "ssh://"+alias); err != nil {
		clearSecret(pending.privateKeyPEM)
		return localapi.Device{}, unavailable("cannot create managed Docker context")
	}
	if err := c.options.Secrets.Put(remoteDeviceID, sshtransport.SSHPrivateKeyCredential, pending.privateKeyPEM); err != nil {
		clearSecret(pending.privateKeyPEM)
		return localapi.Device{}, unavailable("cannot store paired SSH identity")
	}
	clearSecret(pending.privateKeyPEM)

	cfg, err := loadAgentConfig(c.options.Store)
	if err != nil {
		_ = c.options.Secrets.Delete(remoteDeviceID, sshtransport.SSHPrivateKeyCredential)
		return localapi.Device{}, unavailable("cannot read paired device configuration")
	}
	if cfg.Devices == nil {
		cfg.Devices = make(map[string]config.Device)
	}
	device := config.Device{
		Name: pending.target.Name, Address: pending.target.Address,
		SSHPort: record.SSHPort, SyncPort: record.SyncthingPort,
		SSHHostPublicKey: record.SSHHostPublicKey, SyncthingDeviceID: record.SyncthingDeviceID,
		ClientDeviceID: pending.clientDeviceID,
	}
	cfg.SchemaVersion = config.CurrentSchemaVersion
	cfg.ActiveDevice = remoteDeviceID
	cfg.Devices[remoteDeviceID] = device
	if err := c.options.Store.Save(cfg); err != nil {
		_ = c.options.Secrets.Delete(remoteDeviceID, sshtransport.SSHPrivateKeyCredential)
		return localapi.Device{}, unavailable("cannot save paired device configuration")
	}
	c.mu.Lock()
	if c.pending != nil {
		clearSecret(c.pending.privateKeyPEM)
	}
	c.pending = nil
	c.mu.Unlock()
	return localapi.Device{ID: remoteDeviceID, Name: device.Name, Address: device.Address}, nil
}

func (c *macPairingCoordinator) Approve(context.Context, string) (localapi.PairingStatusResult, error) {
	return localapi.PairingStatusResult{}, needsAction("only the Windows host can approve pairing")
}

func (c *macPairingCoordinator) Reject(context.Context, string) (localapi.PairingStatusResult, error) {
	return localapi.PairingStatusResult{}, needsAction("only the Windows host can reject pairing")
}

func (c *macPairingCoordinator) Cancel(ctx context.Context, sessionID string) (localapi.PairingStatusResult, error) {
	c.mu.Lock()
	if c.pending == nil || c.pending.descriptor.ID != sessionID {
		c.mu.Unlock()
		return localapi.PairingStatusResult{}, needsAction("pairing session is not active")
	}
	if c.pending.completing {
		c.mu.Unlock()
		return localapi.PairingStatusResult{}, needsAction("pairing approval is being completed")
	}
	pending := *c.pending
	pending.privateKeyPEM = append([]byte(nil), c.pending.privateKeyPEM...)
	c.mu.Unlock()
	if err := c.options.Transport.Cancel(ctx, pending.target, pending.descriptor); err != nil {
		clearSecret(pending.privateKeyPEM)
		return localapi.PairingStatusResult{}, unavailable("cannot cancel the pairing request")
	}
	c.clearPending(sessionID)
	clearSecret(pending.privateKeyPEM)
	return pairingStatusResult(pending, pairing.SessionStatus{
		SessionID: sessionID, State: pairing.SessionCancelled, ExpiresAt: pending.descriptor.ExpiresAt,
	}), nil
}

func (c *macPairingCoordinator) clearPending(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending != nil && c.pending.descriptor.ID == sessionID {
		clearSecret(c.pending.privateKeyPEM)
		c.pending = nil
	}
}

func pairingStatusResult(pending pendingPairing, status pairing.SessionStatus) localapi.PairingStatusResult {
	code, _ := pairing.Code(pending.descriptor)
	return localapi.PairingStatusResult{
		SessionID: status.SessionID,
		Peer:      localapi.LifecyclePeer{ID: pending.target.InstanceID, Name: pending.target.Name, OS: "windows", Address: pending.target.Address},
		Code:      code, Status: string(status.State), ExpiresAt: status.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
}

func (c *macPairingCoordinator) Unpair(ctx context.Context, deviceID string) error {
	cfg, err := loadAgentConfig(c.options.Store)
	if err != nil {
		return unavailable("cannot read paired device configuration")
	}
	if deviceID == "" {
		deviceID = cfg.ActiveDevice
	}
	device, exists := cfg.Devices[deviceID]
	if !exists || strings.TrimSpace(device.ClientDeviceID) == "" {
		return needsAction("paired device was not found")
	}
	if err := c.options.Transport.Revoke(ctx, device, device.ClientDeviceID); err != nil {
		return unavailable("remote pairing revocation failed")
	}
	delete(cfg.Devices, deviceID)
	if cfg.ActiveDevice == deviceID {
		cfg.ActiveDevice = ""
	}
	if err := c.options.Store.Save(cfg); err != nil {
		return unavailable("cannot save pairing revocation")
	}
	if err := c.options.Secrets.Delete(deviceID, sshtransport.SSHPrivateKeyCredential); err != nil && !errors.Is(err, credentials.ErrNotFound) {
		return unavailable("cannot delete revoked SSH identity")
	}
	return nil
}

type discoveryPairingTransport struct {
	SSHConfigPath string
	SSHBinary     string
	DialContext   systemtransport.DialContextFunc
	discover      func(context.Context) ([]discovery.Peer, error)
	inspect       func(context.Context, string, string) (pairing.Info, error)
	bootstrap     func(context.Context, string, ed25519.PublicKey) (pairing.SessionDescriptor, error)
}

func (t discoveryPairingTransport) Candidates(ctx context.Context) ([]pairingTarget, error) {
	peers, err := t.discoverPeers(ctx)
	if err != nil {
		return nil, err
	}
	targets := make([]pairingTarget, 0, len(peers))
	for _, peer := range peers {
		if !peer.Pairing || len(peer.Addresses) == 0 {
			continue
		}
		target, inspectErr := t.inspectPeer(ctx, peer, peer.Addresses)
		if inspectErr == nil {
			targets = append(targets, target)
		}
	}
	return targets, nil
}

func (t discoveryPairingTransport) Bootstrap(ctx context.Context, selector string, clientPublicKey ed25519.PublicKey) (pairingTarget, pairing.SessionDescriptor, error) {
	if strings.TrimSpace(selector) == "" {
		return pairingTarget{}, pairing.SessionDescriptor{}, errors.New("select a pairing peer before starting")
	}
	peers, err := t.discoverPeers(ctx)
	if err != nil {
		return pairingTarget{}, pairing.SessionDescriptor{}, err
	}
	peer, addresses, err := selectPairingPeer(peers, selector)
	if err != nil {
		return pairingTarget{}, pairing.SessionDescriptor{}, err
	}
	target, err := t.inspectPeer(ctx, peer, addresses)
	if err != nil {
		return pairingTarget{}, pairing.SessionDescriptor{}, err
	}
	bootstrap := t.bootstrap
	if bootstrap == nil {
		bootstrap = func(ctx context.Context, endpoint string, key ed25519.PublicKey) (pairing.SessionDescriptor, error) {
			return pairing.Bootstrap(ctx, endpoint, key, pairing.NewDiscoveryHTTPClient(15*time.Second, t.DialContext))
		}
	}
	endpoint := pairingEndpoint(target)
	descriptor, err := bootstrap(ctx, endpoint, clientPublicKey)
	if err != nil {
		return pairingTarget{}, pairing.SessionDescriptor{}, fmt.Errorf("bootstrap discovered pairing peer: %w", err)
	}
	if subtle.ConstantTimeCompare(descriptor.ServerPublicKey, target.ServerPublicKey) != 1 {
		return pairingTarget{}, pairing.SessionDescriptor{}, errors.New("pairing TLS identity changed after selection")
	}
	return target, descriptor, nil
}

func (t discoveryPairingTransport) inspectPeer(ctx context.Context, peer discovery.Peer, addresses []net.IP) (pairingTarget, error) {
	inspect := t.inspect
	if inspect == nil {
		inspect = func(ctx context.Context, endpoint, instanceID string) (pairing.Info, error) {
			return pairing.Inspect(ctx, endpoint, instanceID, pairing.NewDiscoveryHTTPClient(5*time.Second, t.DialContext))
		}
	}
	var lastErr error
	for _, address := range addresses {
		target := pairingTarget{InstanceID: peer.InstanceID, Address: address.String(), PairingPort: peer.Port}
		info, err := inspect(ctx, pairingEndpoint(target), peer.InstanceID)
		if err == nil {
			target.Name = info.DisplayName
			target.ServerPublicKey = append(ed25519.PublicKey(nil), info.ServerPublicKey...)
			return target, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return pairingTarget{}, ctx.Err()
		}
	}
	return pairingTarget{}, fmt.Errorf("inspect discovered pairing peer: %w", lastErr)
}

func pairingEndpoint(target pairingTarget) string {
	return "https://" + net.JoinHostPort(target.Address, fmt.Sprintf("%d", target.PairingPort))
}

func (t discoveryPairingTransport) discoverPeers(ctx context.Context) ([]discovery.Peer, error) {
	if t.discover != nil {
		return t.discover(ctx)
	}
	browser, err := discovery.NewBrowser()
	if err != nil {
		return nil, err
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, pairingDiscoveryTimeout)
	defer cancel()
	return (discovery.Service{Browser: browser}).Discover(discoveryCtx)
}

func (t discoveryPairingTransport) Confirm(ctx context.Context, target pairingTarget, descriptor pairing.SessionDescriptor, clientDeviceID, authorizedKey, code string) (pairing.DeviceRecord, error) {
	endpoint := "https://" + net.JoinHostPort(target.Address, fmt.Sprintf("%d", target.PairingPort))
	client := pairing.Client{
		BaseURL: endpoint, Session: descriptor, DeviceID: clientDeviceID, AuthorizedKey: authorizedKey,
		HTTPClient: pairing.NewPinnedHTTPClient(descriptor.ServerPublicKey, t.DialContext),
	}
	record, _, err := client.Confirm(ctx, code)
	return record, err
}

func (t discoveryPairingTransport) Status(ctx context.Context, target pairingTarget, descriptor pairing.SessionDescriptor) (pairing.SessionStatus, error) {
	client := pairing.Client{
		BaseURL: pairingEndpoint(target), Session: descriptor,
		HTTPClient: pairing.NewPinnedHTTPClient(descriptor.ServerPublicKey, t.DialContext),
	}
	return client.Status(ctx)
}

func (t discoveryPairingTransport) Cancel(ctx context.Context, target pairingTarget, descriptor pairing.SessionDescriptor) error {
	client := pairing.Client{
		BaseURL: pairingEndpoint(target), Session: descriptor,
		HTTPClient: pairing.NewPinnedHTTPClient(descriptor.ServerPublicKey, t.DialContext),
	}
	return client.Cancel(ctx)
}

func (t discoveryPairingTransport) Revoke(ctx context.Context, device config.Device, clientDeviceID string) error {
	alias, err := pairedRemoteDeviceID(device.SSHHostPublicKey)
	if err != nil {
		return err
	}
	binary := t.SSHBinary
	if binary == "" {
		binary = "ssh"
	}
	request := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "pairing.revoke",
		"params": map[string]string{"device_id": clientDeviceID},
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	var output bytes.Buffer
	command := sshtransport.Command{
		Binary: binary,
		Args:   []string{"-F", t.SSHConfigPath, "remote-docker-device-" + alias, "remote-docker-remote", "rpc"},
		Stdin:  bytes.NewReader(append(encoded, '\n')), Stdout: &output, Stderr: io.Discard,
	}
	if err := runSSHCommand(ctx, command); err != nil {
		return err
	}
	var response struct {
		Result map[string]bool `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(&output, 64<<10)).Decode(&response); err != nil || len(response.Error) != 0 || !response.Result["revoked"] {
		return errors.New("managed pairing revocation was not acknowledged")
	}
	return nil
}

func selectPairingPeer(peers []discovery.Peer, selector string) (discovery.Peer, []net.IP, error) {
	type candidate struct {
		peer      discovery.Peer
		addresses map[string]net.IP
	}
	selectorIP := net.ParseIP(selector)
	candidates := make(map[string]*candidate)
	for _, peer := range peers {
		if !peer.Pairing {
			continue
		}
		if selector != "" && selectorIP == nil && selector != peer.InstanceID {
			continue
		}
		for _, address := range peer.Addresses {
			if address == nil || (!address.IsPrivate() && !address.IsLoopback()) ||
				(selectorIP != nil && !address.Equal(selectorIP)) {
				continue
			}
			selected := candidates[peer.InstanceID]
			if selected == nil {
				selected = &candidate{peer: peer, addresses: make(map[string]net.IP)}
				candidates[peer.InstanceID] = selected
			}
			selected.addresses[address.String()] = append(net.IP(nil), address...)
		}
	}
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(candidates) == 0 {
		return discovery.Peer{}, nil, errors.New("no matching pairing peer was discovered")
	}
	if len(candidates) != 1 {
		return discovery.Peer{}, nil, errors.New("multiple pairing peers were discovered; select one")
	}
	selected := candidates[ids[0]]
	addressStrings := make([]string, 0, len(selected.addresses))
	for address := range selected.addresses {
		addressStrings = append(addressStrings, address)
	}
	sort.Strings(addressStrings)
	addresses := make([]net.IP, 0, len(addressStrings))
	for _, address := range addressStrings {
		addresses = append(addresses, selected.addresses[address])
	}
	return selected.peer, addresses, nil
}

func pairedRemoteDeviceID(hostPublicKey string) (string, error) {
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(hostPublicKey))
	if err != nil || key.Type() != ssh.KeyAlgoED25519 || len(bytes.TrimSpace(rest)) != 0 {
		return "", errors.New("paired SSH host key is invalid")
	}
	return discovery.DeviceIDFromHostKey(key.Marshal()), nil
}

func randomDeviceID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func loadAgentConfig(store config.Store) (config.Config, error) {
	cfg, err := store.Load()
	if errors.Is(err, os.ErrNotExist) {
		return config.Config{SchemaVersion: config.CurrentSchemaVersion}, nil
	}
	if err != nil {
		return config.Config{}, err
	}
	if cfg.SchemaVersion != config.CurrentSchemaVersion {
		return config.Config{}, errors.New("configuration schema version is unsupported")
	}
	return cfg, nil
}

func unavailable(message string) *localapi.PublicError {
	return &localapi.PublicError{Code: localapi.ErrorUnavailable, Message: message}
}

func needsAction(message string) *localapi.PublicError {
	return &localapi.PublicError{Code: localapi.ErrorNeedsAction, Message: message}
}

func clearSecret(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func runSSHCommand(ctx context.Context, command sshtransport.Command) error {
	process := exec.CommandContext(ctx, command.Binary, command.Args...)
	process.Env = command.Env
	process.Stdin = command.Stdin
	process.Stdout = command.Stdout
	process.Stderr = command.Stderr
	return process.Run()
}

func defaultRuntimePaths(configPath string) (sshConfig, knownHosts, agentSocket, controlDir string) {
	root := filepath.Dir(configPath)
	controlRoot := filepath.Join(root, "run")
	if privateRoot := defaultPrivateRuntimeRoot(); privateRoot != "" {
		controlRoot = privateRoot
	}
	return filepath.Join(root, "ssh_config"), filepath.Join(root, "known_hosts"),
		filepath.Join(root, "run", "ssh-agent.sock"), filepath.Join(controlRoot, "ssh-control")
}

// DefaultSSHConfigPath returns the per-user config owned by the installed SSH
// adapter used only by Remote Docker child processes.
func DefaultSSHConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	sshConfig, _, _, _ := defaultRuntimePaths(config.DefaultPath(runtime.GOOS, home))
	return sshConfig, nil
}
