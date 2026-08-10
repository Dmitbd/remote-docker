package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
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
	"github.com/Dmitbd/remote-docker/internal/tunnel"
	"golang.org/x/crypto/ssh"
)

const pairingDiscoveryTimeout = 3 * time.Second

const revocationProofCredential = "pairing-revocation-proof"

type pairingTarget struct {
	InstanceID           string
	Name                 string
	Address              string
	PairingPort          int
	ServerPublicKey      ed25519.PublicKey
	TrustedAdvertisement bool
}

type pairingTransport interface {
	Candidates(context.Context) ([]pairingTarget, error)
	Bootstrap(context.Context, string, ed25519.PublicKey) (pairingTarget, pairing.SessionDescriptor, error)
	Status(context.Context, pairingTarget, pairing.SessionDescriptor) (pairing.SessionStatus, error)
	Cancel(context.Context, pairingTarget, pairing.SessionDescriptor) error
	Confirm(context.Context, pairingTarget, pairing.SessionDescriptor, string, string, string, []byte) (pairing.DeviceRecord, error)
	Revoke(context.Context, config.Device, string, []byte) error
}

type macPairingOptions struct {
	Store                   config.Store
	ConfigTransactions      *configTransactions
	Secrets                 credentials.Store
	Transport               pairingTransport
	Docker                  dockercli.Executor
	DockerCLI               string
	DockerContext           string
	SSHConfigPath           string
	ManagedSSHRoot          sshtransport.ManagedRoot
	KnownHostsPath          string
	AgentSocketPath         string
	ControlDir              string
	ClientDeviceID          func(context.Context) (string, error)
	RemovePinnedHost        func(string, string) error
	RemoveSSHConfig         func(sshtransport.ManagedRoot, string) error
	SaveConfig              func(config.Config) error
	BeforeConfigTransaction func()
}

type pendingPairing struct {
	target          pairingTarget
	descriptor      pairing.SessionDescriptor
	clientDeviceID  string
	authorizedKey   string
	privateKeyPEM   []byte
	tunnelIdentity  []byte
	revocationProof []byte
	revocationOwner string
	cleanupID       string
	completing      bool
	record          *pairing.DeviceRecord
}

type pendingCancellation struct {
	target     pairingTarget
	descriptor pairing.SessionDescriptor
}

type macPairingCoordinator struct {
	options            macPairingOptions
	mu                 sync.Mutex
	pending            *pendingPairing
	cancellation       *pendingCancellation
	starting           bool
	previousDockerHost string
	completingSession  string
}

func newMacPairingCoordinator(options macPairingOptions) *macPairingCoordinator {
	if options.ConfigTransactions == nil {
		options.ConfigTransactions = &configTransactions{}
	}
	if options.RemovePinnedHost == nil {
		options.RemovePinnedHost = sshtransport.RemovePinnedHost
	}
	if options.RemoveSSHConfig == nil {
		options.RemoveSSHConfig = func(root sshtransport.ManagedRoot, path string) error {
			return root.RemoveConfig(path)
		}
	}
	if options.SaveConfig == nil {
		options.SaveConfig = options.Store.Save
	}
	return &macPairingCoordinator{options: options}
}

func (c *macPairingCoordinator) Candidates(ctx context.Context) (localapi.PairCandidatesResult, error) {
	if c == nil {
		return localapi.PairCandidatesResult{}, unavailable("pairing discovery is unavailable")
	}
	cfg, err := loadAgentConfig(c.options.Store)
	if err != nil {
		return localapi.PairCandidatesResult{}, unavailable("cannot read paired device configuration")
	}
	activeID := strings.TrimSpace(cfg.ActiveDevice)
	activeDevice, hasTrustedDevice := cfg.Devices[activeID]
	result := localapi.PairCandidatesResult{Candidates: make([]localapi.PairingCandidate, 0, 1)}
	if hasTrustedDevice {
		result.Candidates = append(result.Candidates, localapi.PairingCandidate{
			ID: activeID, Name: activeDevice.Name, Trusted: true,
		})
	}
	if c.options.Transport == nil {
		if hasTrustedDevice {
			return result, nil
		}
		return localapi.PairCandidatesResult{}, unavailable("pairing discovery is unavailable")
	}
	targets, err := c.options.Transport.Candidates(ctx)
	if err != nil {
		if hasTrustedDevice {
			return result, nil
		}
		return localapi.PairCandidatesResult{}, unavailable("cannot discover pairing devices")
	}
	newCandidates := make([]localapi.PairingCandidate, 0, len(targets))
	for _, target := range targets {
		if target.TrustedAdvertisement {
			if hasTrustedDevice && target.InstanceID == activeID {
				result.Candidates[0].Available = true
			}
			continue
		}
		if strings.TrimSpace(target.InstanceID) == "" || strings.TrimSpace(target.Name) == "" {
			continue
		}
		newCandidates = append(newCandidates, localapi.PairingCandidate{
			ID: target.InstanceID, Name: target.Name, Unverified: true, Available: true,
		})
	}
	sort.Slice(newCandidates, func(i, j int) bool {
		if newCandidates[i].Name == newCandidates[j].Name {
			return newCandidates[i].ID < newCandidates[j].ID
		}
		return newCandidates[i].Name < newCandidates[j].Name
	})
	result.Candidates = append(result.Candidates, newCandidates...)
	return result, nil
}

func (c *macPairingCoordinator) Start(ctx context.Context, selector string) (localapi.PairStartResult, error) {
	if c == nil || c.options.Secrets == nil || c.options.Transport == nil || c.options.Docker == nil {
		return localapi.PairStartResult{}, unavailable("pairing infrastructure is unavailable")
	}
	c.mu.Lock()
	var staleCleanupID string
	if c.starting || c.pending != nil && time.Now().Before(c.pending.descriptor.ExpiresAt) {
		c.mu.Unlock()
		return localapi.PairStartResult{}, needsAction("a pairing session is already active")
	}
	if c.pending != nil {
		staleCleanupID = c.pending.cleanupID
		clearPendingSecrets(c.pending)
		c.pending = nil
	}
	cancellation := c.cancellation
	if cancellation != nil && !time.Now().Before(cancellation.descriptor.ExpiresAt) {
		c.cancellation = nil
		cancellation = nil
	}
	c.starting = true
	c.mu.Unlock()
	if staleCleanupID != "" {
		_ = c.requestPendingCleanup(staleCleanupID)
	}
	defer func() {
		c.mu.Lock()
		c.starting = false
		c.mu.Unlock()
	}()
	if cancellation != nil {
		if err := c.cancelDetached(cancellation.target, cancellation.descriptor); err != nil {
			return localapi.PairStartResult{}, unavailable("previous pairing cancellation is still pending")
		}
		c.mu.Lock()
		if c.cancellation == cancellation {
			c.cancellation = nil
		}
		c.mu.Unlock()
	}

	clientPublicKey, clientPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return localapi.PairStartResult{}, unavailable("cannot create pairing identity")
	}
	sshPublicKey, err := ssh.NewPublicKey(clientPublicKey)
	if err != nil {
		clearSecret(clientPrivateKey)
		return localapi.PairStartResult{}, unavailable("cannot create SSH pairing identity")
	}
	tunnelIdentity, err := tunnel.EncodeIdentity(tunnel.Identity{PrivateKey: clientPrivateKey, PublicKey: clientPublicKey})
	if err != nil {
		clearSecret(clientPrivateKey)
		return localapi.PairStartResult{}, unavailable("cannot encode tunnel pairing identity")
	}
	privateBlock, err := ssh.MarshalPrivateKey(clientPrivateKey, "remote-docker managed key")
	clearSecret(clientPrivateKey)
	if err != nil {
		clearSecret(tunnelIdentity)
		return localapi.PairStartResult{}, unavailable("cannot encode SSH pairing identity")
	}
	privateKeyPEM := pem.EncodeToMemory(privateBlock)
	revocationProof := make([]byte, pairing.RevocationProofSize)
	if _, err := rand.Read(revocationProof); err != nil {
		clearSecret(privateKeyPEM)
		clearSecret(tunnelIdentity)
		return localapi.PairStartResult{}, unavailable("cannot create pairing revocation proof")
	}
	clientDeviceIDProvider := c.options.ClientDeviceID
	if clientDeviceIDProvider == nil {
		clientDeviceIDProvider = func(context.Context) (string, error) { return randomDeviceID() }
	}
	clientDeviceID, err := clientDeviceIDProvider(ctx)
	if err != nil || strings.TrimSpace(clientDeviceID) == "" {
		clearSecret(privateKeyPEM)
		clearSecret(tunnelIdentity)
		clearSecret(revocationProof)
		return localapi.PairStartResult{}, unavailable("cannot create client device identity")
	}
	target, descriptor, err := c.options.Transport.Bootstrap(ctx, selector, clientPublicKey)
	if err != nil {
		clearSecret(privateKeyPEM)
		clearSecret(tunnelIdentity)
		clearSecret(revocationProof)
		return localapi.PairStartResult{}, unavailable("cannot start private-LAN pairing session")
	}
	code, err := pairing.Code(descriptor)
	if err != nil {
		clearSecret(privateKeyPEM)
		clearSecret(tunnelIdentity)
		clearSecret(revocationProof)
		if cancelErr := c.cancelDetached(target, descriptor); cancelErr != nil {
			c.mu.Lock()
			c.cancellation = &pendingCancellation{target: target, descriptor: descriptor}
			c.mu.Unlock()
			return localapi.PairStartResult{}, unavailable("invalid pairing session cancellation is pending")
		}
		return localapi.PairStartResult{}, unavailable("pairing session is invalid")
	}
	cleanupID := pairing.InstanceIDFromPublicKey(descriptor.ServerPublicKey)
	revocationOwner := "pairing-session-" + descriptor.ID
	journalDevice := config.Device{
		Name: target.Name, Address: target.Address, ClientDeviceID: clientDeviceID,
		TunnelPort: tunnel.TunnelPort, TunnelPeerPublicKey: tunnel.EncodePublicKey(descriptor.ServerPublicKey),
		TransportVersion: tunnel.CurrentTransportVersion, RevocationCredentialOwner: revocationOwner,
	}
	if cleanupID == "" || c.options.Secrets.Put(revocationOwner, revocationProofCredential, revocationProof) != nil {
		clearPendingSecrets(&pendingPairing{privateKeyPEM: privateKeyPEM, tunnelIdentity: tunnelIdentity, revocationProof: revocationProof})
		_ = c.cancelDetached(target, descriptor)
		return localapi.PairStartResult{}, unavailable("cannot persist pairing rollback proof")
	}
	if err := c.persistPendingRevocation(cleanupID, config.PendingRevocation{
		Device: journalDevice, SessionID: descriptor.ID,
	}); err != nil {
		_ = c.options.Secrets.Delete(revocationOwner, revocationProofCredential)
		clearPendingSecrets(&pendingPairing{privateKeyPEM: privateKeyPEM, tunnelIdentity: tunnelIdentity, revocationProof: revocationProof})
		_ = c.cancelDetached(target, descriptor)
		return localapi.PairStartResult{}, err
	}
	pending := &pendingPairing{
		target: target, descriptor: descriptor, clientDeviceID: clientDeviceID,
		authorizedKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublicKey))),
		privateKeyPEM: privateKeyPEM, tunnelIdentity: tunnelIdentity,
		revocationProof: revocationProof, revocationOwner: revocationOwner, cleanupID: cleanupID,
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

func (c *macPairingCoordinator) cancelDetached(target pairingTarget, descriptor pairing.SessionDescriptor) error {
	cancelCtx, cancel := context.WithTimeout(context.Background(), pairingRollbackTimeout)
	defer cancel()
	return c.options.Transport.Cancel(cancelCtx, target, descriptor)
}

func (c *macPairingCoordinator) Status(ctx context.Context, sessionID string) (localapi.PairingStatusResult, error) {
	return c.readStatus(ctx, sessionID, true)
}

func (c *macPairingCoordinator) Observe(ctx context.Context, sessionID string) (localapi.PairingStatusResult, error) {
	return c.readStatus(ctx, sessionID, false)
}

func (c *macPairingCoordinator) readStatus(ctx context.Context, sessionID string, allowCompletion bool) (localapi.PairingStatusResult, error) {
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
	pending.tunnelIdentity = append([]byte(nil), c.pending.tunnelIdentity...)
	pending.revocationProof = append([]byte(nil), c.pending.revocationProof...)
	defer clearPendingSecrets(&pending)
	if c.pending.record != nil {
		record := *c.pending.record
		record.AuthorizedKeys = append([]string(nil), c.pending.record.AuthorizedKeys...)
		record.TunnelPublicKey = append(ed25519.PublicKey(nil), c.pending.record.TunnelPublicKey...)
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
	if !allowCompletion {
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
	c.completingSession = sessionID
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.completingSession == sessionID {
			c.completingSession = ""
		}
		c.mu.Unlock()
	}()
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
	fail := func(primary error, change dockercli.ContextChange) (localapi.Device, error) {
		_ = c.requestPendingCleanup(pending.cleanupID)
		if change.Name != "" {
			_ = c.updatePendingContext(pending.cleanupID, contextChangeToConfig(change))
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), pairingRollbackTimeout)
		defer cancel()
		rollbackErr := c.cleanupPendingRevocation(rollbackCtx, pending.cleanupID, false, change)
		clearPendingSecrets(&pending)
		c.clearPending(sessionID)
		return localapi.Device{}, errors.Join(primary, rollbackErr)
	}
	var record pairing.DeviceRecord
	if pending.record == nil {
		wantCode, err := pairing.Code(pending.descriptor)
		if err != nil {
			return fail(unavailable("pairing session is invalid"), dockercli.ContextChange{})
		}
		record, err = c.options.Transport.Confirm(
			ctx, pending.target, pending.descriptor, pending.clientDeviceID, pending.authorizedKey, wantCode, pending.revocationProof,
		)
		if err != nil {
			return fail(unavailable("pairing confirmation failed"), dockercli.ContextChange{})
		}
		c.mu.Lock()
		if c.pending != nil && c.pending.descriptor.ID == sessionID {
			stored := record
			stored.AuthorizedKeys = append([]string(nil), record.AuthorizedKeys...)
			stored.TunnelPublicKey = append(ed25519.PublicKey(nil), record.TunnelPublicKey...)
			c.pending.record = &stored
		}
		c.mu.Unlock()
	} else {
		record = *pending.record
		record.AuthorizedKeys = append([]string(nil), pending.record.AuthorizedKeys...)
		record.TunnelPublicKey = append(ed25519.PublicKey(nil), pending.record.TunnelPublicKey...)
	}
	remoteDeviceID, deviceIDErr := pairedRemoteDeviceID(record.SSHHostPublicKey)
	device := config.Device{
		Name: pending.target.Name, Address: pending.target.Address,
		SSHPort: record.SSHPort, SyncPort: record.SyncthingPort,
		SSHHostPublicKey: record.SSHHostPublicKey, SyncthingDeviceID: record.SyncthingDeviceID,
		ClientDeviceID: pending.clientDeviceID,
		TunnelPort:     tunnel.TunnelPort, TunnelPeerPublicKey: tunnel.EncodePublicKey(pending.descriptor.ServerPublicKey),
		TransportVersion: record.TransportVersion, RevocationCredentialOwner: pending.revocationOwner,
	}
	if err := c.updatePendingDevice(pending.cleanupID, remoteDeviceID, device); err != nil {
		return fail(err, dockercli.ContextChange{})
	}
	if deviceIDErr != nil || strings.TrimSpace(record.SyncthingDeviceID) == "" ||
		record.SSHPort < 1 || record.SSHPort > 65535 || record.SyncthingPort < 1 || record.SyncthingPort > 65535 ||
		record.TunnelPort != tunnel.TunnelPort || record.TransportVersion != tunnel.CurrentTransportVersion ||
		len(record.TunnelPublicKey) != ed25519.PublicKeySize ||
		subtle.ConstantTimeCompare(record.TunnelPublicKey, pending.descriptor.ServerPublicKey) != 1 {
		return fail(unavailable("paired device returned invalid public metadata"), dockercli.ContextChange{})
	}
	alias := "remote-docker-device-" + remoteDeviceID
	if err := sshtransport.PinKnownHost(c.options.KnownHostsPath, alias, record.SSHHostPublicKey); err != nil {
		return fail(unavailable("cannot pin paired SSH identity"), dockercli.ContextChange{})
	}
	if err := sshtransport.WriteConfig(c.options.SSHConfigPath, sshtransport.Config{
		DeviceID: remoteDeviceID, HostName: "127.0.0.1", Port: tunnel.DockerRelayPort,
		AgentSocket: c.options.AgentSocketPath, KnownHostsFile: c.options.KnownHostsPath, ControlDir: c.options.ControlDir,
	}); err != nil {
		return fail(unavailable("cannot write managed SSH configuration"), dockercli.ContextChange{})
	}
	c.mu.Lock()
	expectedPreviousHost := c.previousDockerHost
	c.mu.Unlock()
	contextChange, err := dockercli.EnsureContext(
		ctx, c.options.Docker, c.options.DockerCLI, c.options.DockerContext, "ssh://"+alias, expectedPreviousHost,
	)
	if err != nil {
		return fail(unavailable("cannot create managed Docker context"), dockercli.ContextChange{})
	}
	if err := c.updatePendingContext(pending.cleanupID, contextChangeToConfig(contextChange)); err != nil {
		return fail(err, contextChange)
	}
	if err := c.options.Secrets.Put(remoteDeviceID, sshtransport.SSHPrivateKeyCredential, pending.privateKeyPEM); err != nil {
		return fail(unavailable("cannot store paired SSH identity"), contextChange)
	}
	if err := c.options.Secrets.Put(remoteDeviceID, tunnel.IdentityCredential, pending.tunnelIdentity); err != nil {
		return fail(unavailable("cannot store paired tunnel identity"), contextChange)
	}
	device.DockerContext = contextChangeToConfig(contextChange)
	device.DockerContext.RemoveOnUnpair = contextChange.Created || !contextChange.Changed()
	err = c.options.ConfigTransactions.Run(func() error {
		cfg, err := loadAgentConfig(c.options.Store)
		if err != nil {
			return unavailable("cannot read paired device configuration")
		}
		if cfg.Devices == nil {
			cfg.Devices = make(map[string]config.Device)
		}
		cfg.SchemaVersion = config.CurrentSchemaVersion
		cfg.ActiveDevice = remoteDeviceID
		cfg.Devices[remoteDeviceID] = device
		delete(cfg.PendingRevocations, pending.cleanupID)
		if err := c.options.SaveConfig(cfg); err != nil {
			return unavailable("cannot save paired device configuration")
		}
		return nil
	})
	if err != nil {
		return fail(err, contextChange)
	}
	clearPendingSecrets(&pending)
	c.mu.Lock()
	if c.pending != nil {
		clearPendingSecrets(c.pending)
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
	pending.tunnelIdentity = append([]byte(nil), c.pending.tunnelIdentity...)
	pending.revocationProof = append([]byte(nil), c.pending.revocationProof...)
	c.mu.Unlock()
	cancelErr := c.options.Transport.Cancel(ctx, pending.target, pending.descriptor)
	clearPendingSecrets(&pending)
	if cancelErr != nil {
		return localapi.PairingStatusResult{}, unavailable("cannot cancel the pairing request")
	}
	c.clearPending(sessionID)
	return pairingStatusResult(pending, pairing.SessionStatus{
		SessionID: sessionID, State: pairing.SessionCancelled, ExpiresAt: pending.descriptor.ExpiresAt,
	}), nil
}

func (c *macPairingCoordinator) clearPending(sessionID string) {
	c.mu.Lock()
	var cleanupID string
	if c.pending != nil && c.pending.descriptor.ID == sessionID {
		cleanupID = c.pending.cleanupID
		clearPendingSecrets(c.pending)
		c.pending = nil
	}
	c.mu.Unlock()
	if cleanupID != "" {
		_ = c.requestPendingCleanup(cleanupID)
	}
}

func (c *macPairingCoordinator) Abandon(sessionID string) {
	if c == nil {
		return
	}
	c.clearPending(sessionID)
}

func pairingStatusResult(pending pendingPairing, status pairing.SessionStatus) localapi.PairingStatusResult {
	code, _ := pairing.Code(pending.descriptor)
	return localapi.PairingStatusResult{
		SessionID: status.SessionID,
		Peer:      localapi.LifecyclePeer{ID: pending.target.InstanceID, Name: pending.target.Name, OS: "windows", Address: pending.target.Address},
		Code:      code, Status: string(status.State), ExpiresAt: status.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
}

func (c *macPairingCoordinator) Unpair(ctx context.Context, deviceID string, localOnly bool) error {
	cfg, err := loadAgentConfig(c.options.Store)
	if err != nil {
		return unavailable("cannot read paired device configuration")
	}
	if deviceID == "" {
		deviceID = cfg.ActiveDevice
	}
	device, exists := cfg.Devices[deviceID]
	if !exists {
		if _, pending := cfg.PendingRevocations[deviceID]; pending {
			return c.cleanupPendingRevocation(ctx, deviceID, localOnly, dockercli.ContextChange{})
		}
		return needsAction("paired device was not found")
	}
	if !localOnly {
		if strings.TrimSpace(device.ClientDeviceID) == "" {
			return remoteRevokeUnavailable("remote pairing revocation is unavailable for this saved device")
		}
		proofOwner := device.RevocationCredentialOwner
		if proofOwner == "" {
			proofOwner = deviceID
		}
		if _, err := c.options.Secrets.Get(proofOwner, revocationProofCredential); err != nil {
			return remoteRevokeUnavailable("remote pairing revocation is unavailable for this saved device")
		}
	}
	if err := c.moveActiveToPending(deviceID, device); err != nil {
		return err
	}
	return c.cleanupPendingRevocation(ctx, deviceID, localOnly, dockercli.ContextChange{})
}

func (c *macPairingCoordinator) moveActiveToPending(deviceID string, device config.Device) error {
	if device.DockerContext.Name == "" && c.options.Docker != nil && c.options.DockerContext != "" {
		device.DockerContext = config.DockerContextChange{
			Name: c.options.DockerContext, CurrentHost: "ssh://remote-docker-device-" + deviceID, Created: true,
		}
	}
	if c.options.BeforeConfigTransaction != nil {
		c.options.BeforeConfigTransaction()
	}
	return c.options.ConfigTransactions.Run(func() error {
		cfg, err := loadAgentConfig(c.options.Store)
		if err != nil {
			return unavailable("cannot refresh paired device configuration")
		}
		if cfg.ActiveDevice != deviceID {
			return needsAction("paired device was not found")
		}
		if cfg.PendingRevocations == nil {
			cfg.PendingRevocations = make(map[string]config.PendingRevocation)
		}
		cfg.PendingRevocations[deviceID] = config.PendingRevocation{
			Device: device, DockerContext: device.DockerContext, LocalDeviceID: deviceID, CleanupRequested: true,
		}
		delete(cfg.Devices, deviceID)
		cfg.ActiveDevice = ""
		if err := c.options.SaveConfig(cfg); err != nil {
			return unavailable("cannot save pending pairing removal")
		}
		return nil
	})
}

func (c *macPairingCoordinator) cleanupPendingRevocation(ctx context.Context, deviceID string, localOnly bool, contextOverride dockercli.ContextChange) error {
	cfg, err := loadAgentConfig(c.options.Store)
	if err != nil {
		return unavailable("cannot read pending pairing removal")
	}
	pending, exists := cfg.PendingRevocations[deviceID]
	if !exists {
		return nil
	}
	remoteRevoked := localOnly
	localDeviceID := pending.LocalDeviceID
	proofOwner := pending.Device.RevocationCredentialOwner
	if proofOwner == "" {
		proofOwner = deviceID
	}
	if !localOnly {
		proof, proofErr := c.options.Secrets.Get(proofOwner, revocationProofCredential)
		if proofErr == nil {
			remoteRevoked = c.options.Transport.Revoke(ctx, pending.Device, pending.Device.ClientDeviceID, proof) == nil
			clearSecret(proof)
		}
	}
	change := contextOverride
	if change.Name == "" {
		change = contextChangeFromConfig(pending.DockerContext)
	}
	if change.Name != "" && c.options.Docker != nil {
		if err := dockercli.RestoreContext(ctx, c.options.Docker, c.options.DockerCLI, change); err != nil {
			return unavailable("managed Docker context could not be restored safely")
		}
	}
	if localDeviceID != "" {
		alias := "remote-docker-device-" + localDeviceID
		if err := c.options.RemovePinnedHost(c.options.KnownHostsPath, alias); err != nil {
			return unavailable("cannot remove pinned SSH identity")
		}
		if cfg.ActiveDevice == "" {
			if err := c.options.RemoveSSHConfig(c.options.ManagedSSHRoot, c.options.SSHConfigPath); err != nil {
				return unavailable("cannot remove managed SSH configuration")
			}
		}
		if err := c.options.Secrets.Delete(localDeviceID, sshtransport.SSHPrivateKeyCredential); err != nil && !errors.Is(err, credentials.ErrNotFound) {
			return unavailable("cannot delete paired SSH identity")
		}
		if err := c.options.Secrets.Delete(localDeviceID, tunnel.IdentityCredential); err != nil && !errors.Is(err, credentials.ErrNotFound) {
			return unavailable("cannot delete paired tunnel identity")
		}
	}
	if remoteRevoked {
		if err := c.options.Secrets.Delete(proofOwner, revocationProofCredential); err != nil && !errors.Is(err, credentials.ErrNotFound) {
			return unavailable("cannot delete pairing rollback proof")
		}
	}
	err = c.options.ConfigTransactions.Run(func() error {
		cfg, err := loadAgentConfig(c.options.Store)
		if err != nil {
			return unavailable("cannot refresh pending pairing removal")
		}
		current, exists := cfg.PendingRevocations[deviceID]
		if !exists {
			return nil
		}
		if remoteRevoked {
			delete(cfg.PendingRevocations, deviceID)
		} else {
			current.DockerContext = config.DockerContextChange{}
			current.Device.DockerContext = config.DockerContextChange{}
			cfg.PendingRevocations[deviceID] = current
		}
		if err := c.options.SaveConfig(cfg); err != nil {
			return unavailable("cannot save pending pairing cleanup")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

func (c *macPairingCoordinator) persistPendingRevocation(deviceID string, pending config.PendingRevocation) error {
	return c.options.ConfigTransactions.Run(func() error {
		cfg, err := loadAgentConfig(c.options.Store)
		if err != nil {
			return unavailable("cannot read pairing rollback journal")
		}
		if cfg.PendingRevocations == nil {
			cfg.PendingRevocations = make(map[string]config.PendingRevocation)
		}
		cfg.PendingRevocations[deviceID] = pending
		if err := c.options.SaveConfig(cfg); err != nil {
			return unavailable("cannot save pairing rollback journal")
		}
		return nil
	})
}

func (c *macPairingCoordinator) updatePendingDevice(cleanupID, localDeviceID string, device config.Device) error {
	return c.options.ConfigTransactions.Run(func() error {
		cfg, err := loadAgentConfig(c.options.Store)
		if err != nil {
			return unavailable("cannot read pairing rollback journal")
		}
		pending, ok := cfg.PendingRevocations[cleanupID]
		if !ok {
			return unavailable("pairing rollback journal is missing")
		}
		pending.Device = device
		pending.LocalDeviceID = localDeviceID
		cfg.PendingRevocations[cleanupID] = pending
		if err := c.options.SaveConfig(cfg); err != nil {
			return unavailable("cannot update pairing rollback journal")
		}
		return nil
	})
}

func (c *macPairingCoordinator) updatePendingContext(deviceID string, change config.DockerContextChange) error {
	return c.options.ConfigTransactions.Run(func() error {
		cfg, err := loadAgentConfig(c.options.Store)
		if err != nil {
			return unavailable("cannot read pairing rollback journal")
		}
		pending, ok := cfg.PendingRevocations[deviceID]
		if !ok {
			return unavailable("pairing rollback journal is missing")
		}
		pending.DockerContext = change
		pending.Device.DockerContext = change
		cfg.PendingRevocations[deviceID] = pending
		if err := c.options.SaveConfig(cfg); err != nil {
			return unavailable("cannot save managed Docker rollback")
		}
		return nil
	})
}

func (c *macPairingCoordinator) requestPendingCleanup(deviceID string) error {
	return c.options.ConfigTransactions.Run(func() error {
		cfg, err := loadAgentConfig(c.options.Store)
		if err != nil {
			return unavailable("cannot read pairing rollback journal")
		}
		pending, ok := cfg.PendingRevocations[deviceID]
		if !ok || pending.CleanupRequested {
			return nil
		}
		pending.CleanupRequested = true
		cfg.PendingRevocations[deviceID] = pending
		if err := c.options.SaveConfig(cfg); err != nil {
			return unavailable("cannot activate pairing rollback")
		}
		return nil
	})
}

func (c *macPairingCoordinator) ReconcilePendingRevocations(ctx context.Context) error {
	cfg, err := loadAgentConfig(c.options.Store)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(cfg.PendingRevocations))
	for id, pending := range cfg.PendingRevocations {
		if !pending.CleanupRequested {
			c.mu.Lock()
			leased := pending.SessionID != "" && (c.completingSession == pending.SessionID ||
				c.pending != nil && c.pending.descriptor.ID == pending.SessionID)
			c.mu.Unlock()
			if leased {
				continue
			}
			if err := c.requestPendingCleanup(id); err != nil {
				return err
			}
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := c.cleanupPendingRevocation(ctx, id, false, dockercli.ContextChange{}); err != nil {
			return err
		}
	}
	return nil
}

func contextChangeToConfig(change dockercli.ContextChange) config.DockerContextChange {
	return config.DockerContextChange{Name: change.Name, PreviousHost: change.PreviousHost, CurrentHost: change.CurrentHost, Created: change.Created}
}

func contextChangeFromConfig(change config.DockerContextChange) dockercli.ContextChange {
	if change.RemoveOnUnpair {
		change.PreviousHost = ""
		change.Created = true
	}
	return dockercli.ContextChange{Name: change.Name, PreviousHost: change.PreviousHost, CurrentHost: change.CurrentHost, Created: change.Created}
}

type discoveryPairingTransport struct {
	Store             config.Store
	Secrets           credentials.Store
	SSHConfigPath     string
	DialContext       systemtransport.DialContextFunc
	TunnelDialContext systemtransport.DialContextFunc
	discover          func(context.Context) ([]discovery.Peer, error)
	inspect           func(context.Context, string, string) (pairing.Info, error)
	bootstrap         func(context.Context, string, ed25519.PublicKey) (pairing.SessionDescriptor, error)
	verifySaved       func(context.Context, discovery.Peer) (pairingTarget, error)
}

func (t discoveryPairingTransport) Candidates(ctx context.Context) ([]pairingTarget, error) {
	peers, err := t.discoverPeers(ctx)
	if err != nil {
		return nil, err
	}
	targets := make([]pairingTarget, 0, len(peers))
	for _, peer := range peers {
		if len(peer.Addresses) == 0 {
			continue
		}
		if !peer.Pairing {
			verify := t.verifySaved
			if verify == nil {
				verify = t.verifySavedPeer
			}
			target, verifyErr := verify(ctx, peer)
			if verifyErr == nil {
				targets = append(targets, target)
			}
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
	var target pairingTarget
	selectorIP := net.ParseIP(selector)
	if selectorIP != nil {
		if !selectorIP.IsPrivate() || selectorIP.IsLoopback() || selectorIP.IsUnspecified() {
			return pairingTarget{}, pairing.SessionDescriptor{}, errors.New("manual pairing requires a private IP address")
		}
		inspect := t.inspect
		if inspect == nil {
			inspect = func(ctx context.Context, endpoint, instanceID string) (pairing.Info, error) {
				return pairing.Inspect(ctx, endpoint, instanceID, pairing.NewDiscoveryHTTPClient(5*time.Second, t.DialContext))
			}
		}
		manual := pairingTarget{Address: selectorIP.String(), PairingPort: tunnel.TunnelPort}
		info, err := inspect(ctx, pairingEndpoint(manual), "")
		if err != nil {
			return pairingTarget{}, pairing.SessionDescriptor{}, fmt.Errorf("inspect manual pairing peer: %w", err)
		}
		manual.InstanceID, manual.Name = info.InstanceID, info.DisplayName
		manual.ServerPublicKey = append(ed25519.PublicKey(nil), info.ServerPublicKey...)
		target = manual
	} else {
		peers, err := t.discoverPeers(ctx)
		if err != nil {
			return pairingTarget{}, pairing.SessionDescriptor{}, err
		}
		peer, addresses, err := selectPairingPeer(peers, selector)
		if err != nil {
			return pairingTarget{}, pairing.SessionDescriptor{}, err
		}
		target, err = t.inspectPeer(ctx, peer, addresses)
		if err != nil {
			return pairingTarget{}, pairing.SessionDescriptor{}, err
		}
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
	discoveryCtx, cancel := context.WithTimeout(ctx, pairingDiscoveryTimeout)
	defer cancel()
	service := discovery.Service{UDP: discovery.DiscoverUDP, Saved: t.savedPeers()}
	if err == nil {
		service.Browser = browser
	}
	return service.Discover(discoveryCtx)
}

func (t discoveryPairingTransport) savedPeers() []discovery.Peer {
	if strings.TrimSpace(t.Store.Path) == "" {
		return nil
	}
	cfg, err := loadAgentConfig(t.Store)
	if err != nil || cfg.ActiveDevice == "" {
		return nil
	}
	device, ok := cfg.Devices[cfg.ActiveDevice]
	address := net.ParseIP(device.Address)
	peerPublicKey, keyErr := tunnel.ParsePublicKey(device.TunnelPeerPublicKey)
	if !ok || address == nil || !address.IsPrivate() || address.IsLoopback() ||
		device.TunnelPort != tunnel.TunnelPort || device.TransportVersion != tunnel.CurrentTransportVersion || keyErr != nil {
		return nil
	}
	return []discovery.Peer{{
		InstanceID: pairing.InstanceIDFromPublicKey(peerPublicKey), DeviceID: cfg.ActiveDevice, Port: tunnel.TunnelPort,
		Addresses: []net.IP{address},
	}}
}

func (t discoveryPairingTransport) verifySavedPeer(ctx context.Context, peer discovery.Peer) (pairingTarget, error) {
	deviceID := strings.TrimSpace(peer.DeviceID)
	if deviceID == "" || peer.Port != tunnel.TunnelPort || len(peer.Addresses) == 0 || t.Secrets == nil {
		return pairingTarget{}, errors.New("saved tunnel peer is incomplete")
	}
	cfg, err := loadAgentConfig(t.Store)
	if err != nil {
		return pairingTarget{}, errors.New("cannot load saved tunnel peer")
	}
	device, ok := cfg.Devices[deviceID]
	if !ok || device.TunnelPort != tunnel.TunnelPort || device.TransportVersion != tunnel.CurrentTransportVersion {
		return pairingTarget{}, errors.New("saved tunnel peer is no longer trusted")
	}
	serverPublicKey, err := tunnel.ParsePublicKey(device.TunnelPeerPublicKey)
	if err != nil || pairing.InstanceIDFromPublicKey(serverPublicKey) != peer.InstanceID {
		return pairingTarget{}, errors.New("saved tunnel identity does not match discovery")
	}
	encodedIdentity, err := t.Secrets.Get(deviceID, tunnel.IdentityCredential)
	if err != nil {
		return pairingTarget{}, errors.New("saved tunnel client identity is unavailable")
	}
	defer clearSecret(encodedIdentity)
	clientIdentity, err := tunnel.IdentityFromPKCS8(encodedIdentity)
	if err != nil {
		return pairingTarget{}, errors.New("saved tunnel client identity is invalid")
	}
	tlsConfig, err := tunnel.ClientTLSConfig(clientIdentity, serverPublicKey)
	if err != nil {
		return pairingTarget{}, errors.New("saved tunnel TLS configuration is invalid")
	}
	dialContext := t.TunnelDialContext
	if dialContext == nil {
		dialContext = systemtransport.TunnelDialContext()
	}
	var lastErr error
	for _, address := range peer.Addresses {
		if address == nil || (!address.IsPrivate() && !address.IsLoopback()) || address.IsUnspecified() {
			continue
		}
		endpoint := net.JoinHostPort(address.String(), fmt.Sprintf("%d", tunnel.TunnelPort))
		connection, dialErr := dialContext(ctx, "tcp", endpoint)
		if dialErr != nil {
			lastErr = dialErr
			continue
		}
		secured := tls.Client(connection, tlsConfig.Clone())
		handshakeErr := secured.HandshakeContext(ctx)
		_ = secured.Close()
		if handshakeErr == nil {
			return pairingTarget{
				InstanceID: deviceID, Name: device.Name, Address: address.String(), PairingPort: tunnel.TunnelPort,
				TrustedAdvertisement: true,
			}, nil
		}
		lastErr = handshakeErr
		if ctx.Err() != nil {
			return pairingTarget{}, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = errors.New("saved tunnel peer has no safe address")
	}
	return pairingTarget{}, fmt.Errorf("verify saved tunnel peer: %w", lastErr)
}

func (t discoveryPairingTransport) Confirm(ctx context.Context, target pairingTarget, descriptor pairing.SessionDescriptor, clientDeviceID, authorizedKey, code string, revocationProof []byte) (pairing.DeviceRecord, error) {
	endpoint := "https://" + net.JoinHostPort(target.Address, fmt.Sprintf("%d", target.PairingPort))
	client := pairing.Client{
		BaseURL: endpoint, Session: descriptor, DeviceID: clientDeviceID, AuthorizedKey: authorizedKey,
		HTTPClient: pairing.NewPinnedHTTPClient(descriptor.ServerPublicKey, t.DialContext), RevocationProof: revocationProof,
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

func (t discoveryPairingTransport) Revoke(ctx context.Context, device config.Device, clientDeviceID string, proof []byte) error {
	serverPublicKey, err := tunnel.ParsePublicKey(device.TunnelPeerPublicKey)
	if err != nil {
		return errors.New("saved pairing revocation endpoint is invalid")
	}
	type endpoint struct {
		address string
		port    int
	}
	endpoints := make([]endpoint, 0, 2)
	if address := strings.TrimSpace(device.Address); net.ParseIP(address) != nil {
		endpoints = append(endpoints, endpoint{address: address, port: tunnel.TunnelPort})
	}
	instanceID := pairing.InstanceIDFromPublicKey(serverPublicKey)
	if peers, discoverErr := t.discoverPeers(ctx); discoverErr == nil {
		for _, peer := range peers {
			if peer.InstanceID != instanceID {
				continue
			}
			for _, address := range peer.Addresses {
				if address != nil && (address.IsPrivate() || address.IsLoopback()) && !address.IsUnspecified() {
					endpoints = append(endpoints, endpoint{address: address.String(), port: peer.Port})
				}
			}
		}
	}
	var lastErr error
	seen := make(map[string]struct{})
	for _, endpoint := range endpoints {
		key := net.JoinHostPort(endpoint.address, fmt.Sprintf("%d", endpoint.port))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		client := pairing.Client{
			BaseURL:    "https://" + key,
			Session:    pairing.SessionDescriptor{ServerPublicKey: serverPublicKey},
			HTTPClient: pairing.NewPinnedHTTPClient(serverPublicKey, t.DialContext),
		}
		client.HTTPClient.Timeout = pairingDiscoveryTimeout
		if err := client.Revoke(ctx, clientDeviceID, proof); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("saved pairing revocation endpoint is unavailable")
	}
	return lastErr
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

func remoteRevokeUnavailable(message string) *localapi.PublicError {
	return &localapi.PublicError{Code: localapi.ErrorRemoteRevokeUnavailable, Message: message}
}

func clearSecret(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func clearPendingSecrets(pending *pendingPairing) {
	if pending == nil {
		return
	}
	clearSecret(pending.privateKeyPEM)
	clearSecret(pending.tunnelIdentity)
	clearSecret(pending.revocationProof)
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
