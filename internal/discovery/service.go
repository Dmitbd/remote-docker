package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"

	"github.com/grandcat/zeroconf"
)

const (
	ServiceType            = "_remote-docker._tcp"
	DefaultProtocolVersion = "1"
)

// Record is the network-neutral subset of one mDNS service entry.
type Record struct {
	Port      int
	TXT       []string
	Addresses []net.IP
}

// Peer is one filtered and deduplicated Windows Agent.
type Peer struct {
	InstanceID string
	DeviceID   string
	Pairing    bool
	Port       int
	Addresses  []net.IP
}

// Browser hides the concrete mDNS implementation from discovery logic.
type Browser interface {
	Browse(ctx context.Context, serviceType string) (<-chan Record, error)
}

// Service filters mDNS records to compatible peers on local addresses.
type Service struct {
	Browser         Browser
	ProtocolVersion string
	UDP             func(context.Context) ([]Peer, error)
	Saved           []Peer
}

// Discover collects valid records until the browser closes or ctx expires.
func (s Service) Discover(ctx context.Context) ([]Peer, error) {
	version := s.ProtocolVersion
	if version == "" {
		version = DefaultProtocolVersion
	}
	var records <-chan Record
	if s.Browser != nil {
		var err error
		records, err = s.Browser.Browse(ctx, ServiceType)
		if err != nil && s.UDP == nil && len(s.Saved) == 0 {
			return nil, fmt.Errorf("browse remote Docker services: %w", err)
		}
	}
	if records == nil {
		closed := make(chan Record)
		close(closed)
		records = closed
	}

	peersByID := make(map[string]*Peer)
	for _, peer := range s.Saved {
		mergePeer(peersByID, peer)
	}
	if s.UDP != nil {
		udpPeers, _ := s.UDP(ctx)
		for _, peer := range udpPeers {
			mergePeer(peersByID, peer)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return sortedPeers(peersByID), nil
		case record, ok := <-records:
			if !ok {
				return sortedPeers(peersByID), nil
			}
			peer, ok := peerFromRecord(record, version)
			if !ok {
				continue
			}
			mergePeer(peersByID, peer)
		}
	}
}

func mergePeer(peers map[string]*Peer, peer Peer) {
	if validateOpaqueID(peer.InstanceID) != nil || !validPort(peer.Port) {
		return
	}
	existing, found := peers[peer.InstanceID]
	if !found {
		copy := peer
		copy.Addresses = mergeAddresses(nil, peer.Addresses)
		peers[peer.InstanceID] = &copy
		return
	}
	if existing.DeviceID == "" && peer.DeviceID != "" {
		existing.DeviceID = peer.DeviceID
		existing.Pairing = false
	} else if existing.DeviceID != "" && peer.DeviceID != "" && existing.DeviceID != peer.DeviceID {
		return
	}
	if existing.Port == 0 {
		existing.Port = peer.Port
	}
	if existing.Port != peer.Port {
		return
	}
	existing.Addresses = mergeAddresses(existing.Addresses, peer.Addresses)
}

// Advertisement contains the only data published over mDNS.
type Advertisement struct {
	InstanceName string
	Port         int
	TXT          []string
}

// PairingAdvertisement publishes only a temporary opaque identity. Display
// metadata is fetched separately over the TLS-bound pairing-info endpoint.
func PairingAdvertisement(instanceID string, port int) (Advertisement, error) {
	if err := validateOpaqueID(instanceID); err != nil {
		return Advertisement{}, fmt.Errorf("invalid pairing instance ID: %w", err)
	}
	if !validPort(port) {
		return Advertisement{}, errors.New("invalid pairing port")
	}
	return Advertisement{
		InstanceName: "remote-docker-" + instanceID,
		Port:         port,
		TXT: []string{
			"version=" + DefaultProtocolVersion,
			"instance=" + instanceID,
			"pairing=1",
		},
	}, nil
}

// PairedAdvertisement creates an advertisement for an already pinned device.
func PairedAdvertisement(deviceID string, port int) (Advertisement, error) {
	if err := validateOpaqueID(deviceID); err != nil {
		return Advertisement{}, fmt.Errorf("invalid device ID: %w", err)
	}
	if !validPort(port) {
		return Advertisement{}, errors.New("invalid service port")
	}
	return Advertisement{
		InstanceName: "remote-docker-" + deviceID,
		Port:         port,
		TXT: []string{
			"version=" + DefaultProtocolVersion,
			"device=" + deviceID,
			"pairing=0",
		},
	}, nil
}

// DeviceIDFromHostKey derives a stable opaque ID from a pinned public host key.
func DeviceIDFromHostKey(hostPublicKey []byte) string {
	digest := sha256.Sum256(hostPublicKey)
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:])
	return encoded[:26]
}

// Registration is a published mDNS service that can be stopped.
type Registration interface {
	Shutdown()
}

// Publisher hides the concrete mDNS publisher from agent code.
type Publisher interface {
	Publish(ctx context.Context, advertisement Advertisement) (Registration, error)
}

// ZeroconfBrowser adapts grandcat/zeroconf to Browser.
type ZeroconfBrowser struct {
	resolver *zeroconf.Resolver
}

// NewZeroconfBrowser creates the production mDNS browser.
func NewZeroconfBrowser() (*ZeroconfBrowser, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, fmt.Errorf("create mDNS resolver: %w", err)
	}
	return &ZeroconfBrowser{resolver: resolver}, nil
}

// Browse starts resolving service entries until ctx is cancelled.
func (b *ZeroconfBrowser) Browse(ctx context.Context, serviceType string) (<-chan Record, error) {
	entries := make(chan *zeroconf.ServiceEntry)
	if err := b.resolver.Browse(ctx, serviceType, "local.", entries); err != nil {
		return nil, fmt.Errorf("start mDNS browse: %w", err)
	}
	records := make(chan Record)
	go func() {
		defer close(records)
		for {
			select {
			case <-ctx.Done():
				return
			case entry, ok := <-entries:
				if !ok {
					return
				}
				addresses := make([]net.IP, 0, len(entry.AddrIPv4)+len(entry.AddrIPv6))
				addresses = append(addresses, entry.AddrIPv4...)
				addresses = append(addresses, entry.AddrIPv6...)
				record := Record{Port: entry.Port, TXT: append([]string(nil), entry.Text...), Addresses: addresses}
				select {
				case records <- record:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return records, nil
}

// ZeroconfPublisher publishes privacy-limited service records.
type ZeroconfPublisher struct{}

// Publish registers an mDNS service and shuts it down with ctx.
func (ZeroconfPublisher) Publish(ctx context.Context, advertisement Advertisement) (Registration, error) {
	if !validPort(advertisement.Port) || advertisement.InstanceName == "" {
		return nil, errors.New("invalid mDNS advertisement")
	}
	server, err := zeroconf.Register(
		advertisement.InstanceName,
		ServiceType,
		"local.",
		advertisement.Port,
		advertisement.TXT,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("publish mDNS service: %w", err)
	}
	registration := &zeroconfRegistration{server: server}
	go func() {
		<-ctx.Done()
		registration.Shutdown()
	}()
	return registration, nil
}

type zeroconfRegistration struct {
	once   sync.Once
	server *zeroconf.Server
}

func (r *zeroconfRegistration) Shutdown() {
	r.once.Do(r.server.Shutdown)
}

func peerFromRecord(record Record, protocolVersion string) (Peer, bool) {
	if !validPort(record.Port) {
		return Peer{}, false
	}
	txt, ok := parseTXT(record.TXT)
	if !ok || txt["version"] != protocolVersion {
		return Peer{}, false
	}
	pairingValue := txt["pairing"]
	peer := Peer{Port: record.Port}
	switch pairingValue {
	case "1":
		if txt["device"] != "" || validateOpaqueID(txt["instance"]) != nil {
			return Peer{}, false
		}
		peer.InstanceID = txt["instance"]
		peer.Pairing = true
	case "0":
		if txt["instance"] != "" || validateOpaqueID(txt["device"]) != nil {
			return Peer{}, false
		}
		peer.InstanceID = txt["device"]
		peer.DeviceID = txt["device"]
	default:
		return Peer{}, false
	}

	for _, address := range record.Addresses {
		if isLocalAddress(address) {
			peer.Addresses = append(peer.Addresses, append(net.IP(nil), address...))
		}
	}
	peer.Addresses = mergeAddresses(nil, peer.Addresses)
	return peer, len(peer.Addresses) > 0
}

func parseTXT(entries []string) (map[string]string, bool) {
	allowed := map[string]bool{"version": true, "instance": true, "device": true, "pairing": true}
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, found := strings.Cut(entry, "=")
		if !found || !allowed[key] || value == "" {
			return nil, false
		}
		if _, duplicate := values[key]; duplicate {
			return nil, false
		}
		values[key] = value
	}
	return values, true
}

func validateOpaqueID(id string) error {
	if id == "" || len(id) > 64 {
		return errors.New("opaque ID must contain 1 to 64 characters")
	}
	for _, character := range id {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' {
			continue
		}
		return errors.New("opaque ID contains an unsupported character")
	}
	return nil
}

func validPort(port int) bool {
	return port > 0 && port <= 65535
}

func isLocalAddress(address net.IP) bool {
	return address != nil && !address.IsUnspecified() &&
		(address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast())
}

func mergeAddresses(existing, additions []net.IP) []net.IP {
	byString := make(map[string]net.IP, len(existing)+len(additions))
	for _, address := range append(existing, additions...) {
		byString[address.String()] = append(net.IP(nil), address...)
	}
	keys := make([]string, 0, len(byString))
	for key := range byString {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]net.IP, 0, len(keys))
	for _, key := range keys {
		result = append(result, byString[key])
	}
	return result
}

func sortedPeers(peersByID map[string]*Peer) []Peer {
	keys := make([]string, 0, len(peersByID))
	for key := range peersByID {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	peers := make([]Peer, 0, len(keys))
	for _, key := range keys {
		peer := *peersByID[key]
		peer.Addresses = mergeAddresses(nil, peer.Addresses)
		peers = append(peers, peer)
	}
	return peers
}

var _ Browser = (*ZeroconfBrowser)(nil)
var _ Publisher = ZeroconfPublisher{}
