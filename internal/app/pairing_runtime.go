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
}

type pendingPairing struct {
	target         pairingTarget
	descriptor     pairing.SessionDescriptor
	clientDeviceID string
	authorizedKey  string
	privateKeyPEM  []byte
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
	clientDeviceID, err := randomDeviceID()
	if err != nil {
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
	return localapi.PairStartResult{SessionID: descriptor.ID, Code: code}, nil
}

func (c *macPairingCoordinator) Confirm(ctx context.Context, params localapi.PairConfirmParams) (localapi.PairConfirmResult, error) {
	if c == nil {
		return localapi.PairConfirmResult{}, unavailable("pairing infrastructure is unavailable")
	}
	c.mu.Lock()
	if c.pending == nil || c.pending.descriptor.ID != params.SessionID {
		c.mu.Unlock()
		return localapi.PairConfirmResult{}, needsAction("pairing session is not active")
	}
	pending := *c.pending
	pending.privateKeyPEM = append([]byte(nil), c.pending.privateKeyPEM...)
	c.mu.Unlock()
	wantCode, err := pairing.Code(pending.descriptor)
	if err != nil || subtle.ConstantTimeCompare([]byte(params.Code), []byte(wantCode)) != 1 {
		clearSecret(pending.privateKeyPEM)
		return localapi.PairConfirmResult{}, needsAction("pairing code does not match")
	}
	record, err := c.options.Transport.Confirm(
		ctx, pending.target, pending.descriptor, pending.clientDeviceID, pending.authorizedKey, params.Code,
	)
	if err != nil {
		clearSecret(pending.privateKeyPEM)
		return localapi.PairConfirmResult{}, unavailable("pairing confirmation failed")
	}
	remoteDeviceID, err := pairedRemoteDeviceID(record.SSHHostPublicKey)
	if err != nil || strings.TrimSpace(record.SyncthingDeviceID) == "" ||
		record.SSHPort < 1 || record.SSHPort > 65535 || record.SyncthingPort < 1 || record.SyncthingPort > 65535 {
		clearSecret(pending.privateKeyPEM)
		return localapi.PairConfirmResult{}, unavailable("paired device returned invalid public metadata")
	}
	alias := "remote-docker-device-" + remoteDeviceID
	if err := sshtransport.PinKnownHost(c.options.KnownHostsPath, alias, record.SSHHostPublicKey); err != nil {
		clearSecret(pending.privateKeyPEM)
		return localapi.PairConfirmResult{}, unavailable("cannot pin paired SSH identity")
	}
	if err := sshtransport.WriteConfig(c.options.SSHConfigPath, sshtransport.Config{
		DeviceID: remoteDeviceID, HostName: pending.target.Address, Port: record.SSHPort,
		AgentSocket: c.options.AgentSocketPath, KnownHostsFile: c.options.KnownHostsPath, ControlDir: c.options.ControlDir,
	}); err != nil {
		clearSecret(pending.privateKeyPEM)
		return localapi.PairConfirmResult{}, unavailable("cannot write managed SSH configuration")
	}
	if err := dockercli.EnsureContext(ctx, c.options.Docker, c.options.DockerCLI, c.options.DockerContext, "ssh://"+alias); err != nil {
		clearSecret(pending.privateKeyPEM)
		return localapi.PairConfirmResult{}, unavailable("cannot create managed Docker context")
	}
	if err := c.options.Secrets.Put(remoteDeviceID, sshtransport.SSHPrivateKeyCredential, pending.privateKeyPEM); err != nil {
		clearSecret(pending.privateKeyPEM)
		return localapi.PairConfirmResult{}, unavailable("cannot store paired SSH identity")
	}
	clearSecret(pending.privateKeyPEM)

	cfg, err := loadAgentConfig(c.options.Store)
	if err != nil {
		_ = c.options.Secrets.Delete(remoteDeviceID, sshtransport.SSHPrivateKeyCredential)
		return localapi.PairConfirmResult{}, unavailable("cannot read paired device configuration")
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
		return localapi.PairConfirmResult{}, unavailable("cannot save paired device configuration")
	}
	c.mu.Lock()
	if c.pending != nil {
		clearSecret(c.pending.privateKeyPEM)
	}
	c.pending = nil
	c.mu.Unlock()
	return localapi.PairConfirmResult{Device: localapi.Device{
		ID: remoteDeviceID, Name: device.Name, Address: device.Address,
	}}, nil
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
			return pairing.Bootstrap(ctx, endpoint, key, nil)
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
			return pairing.Inspect(ctx, endpoint, instanceID, nil)
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
	browser, err := discovery.NewZeroconfBrowser()
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
	}
	record, _, err := client.Confirm(ctx, code)
	return record, err
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
	return filepath.Join(root, "ssh_config"), filepath.Join(root, "known_hosts"),
		filepath.Join(root, "run", "ssh-agent.sock"), filepath.Join(root, "run", "ssh-control")
}
