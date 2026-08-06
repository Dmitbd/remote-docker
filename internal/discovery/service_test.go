package discovery

import (
	"context"
	"net"
	"reflect"
	"sort"
	"testing"
)

func TestDiscoverFiltersAndDeduplicatesRecords(t *testing.T) {
	records := make(chan Record, 8)
	records <- Record{
		Port:      43119,
		TXT:       []string{"version=1", "instance=public", "pairing=1"},
		Addresses: []net.IP{net.ParseIP("8.8.8.8")},
	}
	records <- Record{
		Port:      43119,
		TXT:       []string{"version=2", "instance=wrong-version", "pairing=1"},
		Addresses: []net.IP{net.ParseIP("192.168.1.3")},
	}
	records <- Record{
		Port:      0,
		TXT:       []string{"version=1", "instance=no-port", "pairing=1"},
		Addresses: []net.IP{net.ParseIP("192.168.1.4")},
	}
	records <- Record{
		Port:      43119,
		TXT:       []string{"version=1", "instance=pair-session", "pairing=1"},
		Addresses: []net.IP{net.ParseIP("192.168.1.20")},
	}
	records <- Record{
		Port:      43119,
		TXT:       []string{"version=1", "instance=pair-session", "pairing=1"},
		Addresses: []net.IP{net.ParseIP("fd00::20"), net.ParseIP("192.168.1.20")},
	}
	records <- Record{
		Port:      43119,
		TXT:       []string{"version=1", "device=stable-device", "pairing=0"},
		Addresses: []net.IP{net.ParseIP("10.0.0.20")},
	}
	close(records)

	service := Service{
		Browser:         fakeBrowser{records: records},
		ProtocolVersion: "1",
	}
	peers, err := service.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("Discover() returned %d peers, want 2: %#v", len(peers), peers)
	}

	pairingPeer := findPeer(t, peers, "pair-session")
	if !pairingPeer.Pairing || pairingPeer.DeviceID != "" || pairingPeer.Port != 43119 {
		t.Fatalf("pairing peer = %#v", pairingPeer)
	}
	addresses := ipStrings(pairingPeer.Addresses)
	if want := []string{"192.168.1.20", "fd00::20"}; !reflect.DeepEqual(addresses, want) {
		t.Fatalf("pairing addresses = %#v, want %#v", addresses, want)
	}

	pairedPeer := findPeer(t, peers, "stable-device")
	if pairedPeer.Pairing || pairedPeer.DeviceID != "stable-device" {
		t.Fatalf("paired peer = %#v", pairedPeer)
	}
}

func TestAdvertisementsContainOnlyOpaqueProtocolData(t *testing.T) {
	pairing, err := PairingAdvertisement("session-7f3a", 43119)
	if err != nil {
		t.Fatalf("PairingAdvertisement() error = %v", err)
	}
	if want := []string{"version=1", "instance=session-7f3a", "pairing=1"}; !reflect.DeepEqual(pairing.TXT, want) {
		t.Fatalf("pairing TXT = %#v, want %#v", pairing.TXT, want)
	}

	deviceID := DeviceIDFromHostKey([]byte("ssh-ed25519 pinned-host-public-key"))
	paired, err := PairedAdvertisement(deviceID, 43119)
	if err != nil {
		t.Fatalf("PairedAdvertisement() error = %v", err)
	}
	if want := []string{"version=1", "device=" + deviceID, "pairing=0"}; !reflect.DeepEqual(paired.TXT, want) {
		t.Fatalf("paired TXT = %#v, want %#v", paired.TXT, want)
	}
	if deviceID == "" || deviceID == "ssh-ed25519 pinned-host-public-key" {
		t.Fatalf("DeviceIDFromHostKey() = %q, want opaque ID", deviceID)
	}
}

func TestAdvertisementRejectsIdentifyingOrMalformedValues(t *testing.T) {
	for _, instanceID := range []string{"", "Mark's PC", "../../device", "contains/slash"} {
		if _, err := PairingAdvertisement(instanceID, 43119); err == nil {
			t.Fatalf("PairingAdvertisement(%q) succeeded, want rejection", instanceID)
		}
	}
	if _, err := PairingAdvertisement("opaque-id", 0); err == nil {
		t.Fatal("PairingAdvertisement() accepted missing port")
	}
}

type fakeBrowser struct {
	records <-chan Record
	err     error
}

func (b fakeBrowser) Browse(context.Context, string) (<-chan Record, error) {
	return b.records, b.err
}

func findPeer(t *testing.T, peers []Peer, id string) Peer {
	t.Helper()
	for _, peer := range peers {
		if peer.InstanceID == id {
			return peer
		}
	}
	t.Fatalf("peer %q not found in %#v", id, peers)
	return Peer{}
}

func ipStrings(addresses []net.IP) []string {
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		result = append(result, address.String())
	}
	sort.Strings(result)
	return result
}
