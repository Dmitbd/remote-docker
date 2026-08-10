package app

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/Dmitbd/remote-docker/internal/discovery"
	"github.com/Dmitbd/remote-docker/internal/pairing"
)

func TestDiscoveryPairingCandidatesResolveNameThroughTLSWithoutStartingSession(t *testing.T) {
	serverKey := make(ed25519.PublicKey, ed25519.PublicKeySize)
	inspectCalls, bootstrapCalls := 0, 0
	transport := discoveryPairingTransport{
		discover: func(context.Context) ([]discovery.Peer, error) {
			return []discovery.Peer{{
				InstanceID: "windows-one", Pairing: true, Port: 43119,
				Addresses: []net.IP{net.ParseIP("192.168.1.20")},
			}}, nil
		},
		inspect: func(_ context.Context, endpoint, instanceID string) (pairing.Info, error) {
			inspectCalls++
			if endpoint != "https://192.168.1.20:43119" || instanceID != "windows-one" {
				t.Fatalf("Inspect(%q, %q)", endpoint, instanceID)
			}
			return pairing.Info{InstanceID: instanceID, DisplayName: "Windows Workstation", ServerPublicKey: serverKey}, nil
		},
		bootstrap: func(context.Context, string, ed25519.PublicKey) (pairing.SessionDescriptor, error) {
			bootstrapCalls++
			return pairing.SessionDescriptor{}, nil
		},
	}

	targets, err := transport.Candidates(context.Background())
	if err != nil {
		t.Fatalf("Candidates() error = %v", err)
	}
	want := []pairingTarget{{
		InstanceID: "windows-one", Name: "Windows Workstation", Address: "192.168.1.20",
		PairingPort: 43119, ServerPublicKey: serverKey,
	}}
	if !reflect.DeepEqual(targets, want) || inspectCalls != 1 || bootstrapCalls != 0 {
		t.Fatalf("targets=%#v inspect=%d bootstrap=%d, want %#v/1/0", targets, inspectCalls, bootstrapCalls, want)
	}
}

func TestDiscoveryPairingCandidatesExposeStableTrustedAdvertisementWithoutInferringName(t *testing.T) {
	inspectCalls := 0
	transport := discoveryPairingTransport{
		discover: func(context.Context) ([]discovery.Peer, error) {
			return []discovery.Peer{{
				InstanceID: "trusted-windows", DeviceID: "trusted-windows", Pairing: false, Port: 43119,
				Addresses: []net.IP{net.ParseIP("192.168.1.20")},
			}}, nil
		},
		inspect: func(context.Context, string, string) (pairing.Info, error) {
			inspectCalls++
			return pairing.Info{}, errors.New("stable advertisement must not use a pairing identity")
		},
	}

	targets, err := transport.Candidates(context.Background())
	if err != nil {
		t.Fatalf("Candidates() error = %v", err)
	}
	want := []pairingTarget{{
		InstanceID: "trusted-windows", Address: "192.168.1.20", PairingPort: 43119, TrustedAdvertisement: true,
	}}
	if !reflect.DeepEqual(targets, want) || inspectCalls != 0 {
		t.Fatalf("targets=%#v inspect=%d, want %#v/0", targets, inspectCalls, want)
	}
}

func TestDiscoveryPairingBootstrapRequiresExplicitInstanceAndPinsInspectedTLSKey(t *testing.T) {
	serverKey := make(ed25519.PublicKey, ed25519.PublicKeySize)
	serverKey[0] = 7
	var endpoints []string
	transport := discoveryPairingTransport{
		discover: func(context.Context) ([]discovery.Peer, error) {
			return []discovery.Peer{{
				InstanceID: "windows-one", Pairing: true, Port: 43119,
				Addresses: []net.IP{net.ParseIP("10.0.0.20"), net.ParseIP("192.168.1.20")},
			}}, nil
		},
		inspect: func(_ context.Context, endpoint, instanceID string) (pairing.Info, error) {
			if instanceID != "windows-one" {
				t.Fatalf("instanceID = %q", instanceID)
			}
			return pairing.Info{InstanceID: instanceID, DisplayName: "Windows Workstation", ServerPublicKey: serverKey}, nil
		},
		bootstrap: func(_ context.Context, endpoint string, _ ed25519.PublicKey) (pairing.SessionDescriptor, error) {
			endpoints = append(endpoints, endpoint)
			return pairing.SessionDescriptor{ID: "selected", ServerPublicKey: serverKey}, nil
		},
	}

	if _, _, err := transport.Bootstrap(context.Background(), "", make(ed25519.PublicKey, ed25519.PublicKeySize)); err == nil {
		t.Fatal("Bootstrap() accepted an empty selector")
	}
	target, _, err := transport.Bootstrap(context.Background(), "windows-one", make(ed25519.PublicKey, ed25519.PublicKeySize))
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if target.Address != "10.0.0.20" || target.Name != "Windows Workstation" ||
		!reflect.DeepEqual(target.ServerPublicKey, serverKey) || !reflect.DeepEqual(endpoints, []string{"https://10.0.0.20:43119"}) {
		t.Fatalf("target=%#v endpoints=%#v, want selected TLS-bound peer", target, endpoints)
	}
}

func TestDiscoveryPairingBootstrapRejectsMultipleDistinctInstances(t *testing.T) {
	bootstrapCalls := 0
	transport := discoveryPairingTransport{
		discover: func(context.Context) ([]discovery.Peer, error) {
			return []discovery.Peer{
				{InstanceID: "windows-one", Pairing: true, Port: 43119, Addresses: []net.IP{net.ParseIP("10.0.0.20")}},
				{InstanceID: "windows-two", Pairing: true, Port: 43119, Addresses: []net.IP{net.ParseIP("192.168.1.20")}},
			}, nil
		},
		inspect: func(context.Context, string, string) (pairing.Info, error) {
			return pairing.Info{}, errors.New("should not inspect")
		},
		bootstrap: func(context.Context, string, ed25519.PublicKey) (pairing.SessionDescriptor, error) {
			bootstrapCalls++
			return pairing.SessionDescriptor{}, nil
		},
	}

	_, _, err := transport.Bootstrap(context.Background(), "missing", make(ed25519.PublicKey, ed25519.PublicKeySize))
	if err == nil || !strings.Contains(err.Error(), "matching pairing peer") {
		t.Fatalf("Bootstrap() error = %v, want missing explicit instance", err)
	}
	if bootstrapCalls != 0 {
		t.Fatalf("bootstrap calls = %d, want 0 for ambiguous instances", bootstrapCalls)
	}
}
