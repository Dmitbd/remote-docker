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

func TestDiscoveryPairingBootstrapRetriesAddressesForSingleInstance(t *testing.T) {
	for _, selector := range []string{"", "windows-one"} {
		t.Run("selector="+selector, func(t *testing.T) {
			var endpoints []string
			transport := discoveryPairingTransport{
				discover: func(context.Context) ([]discovery.Peer, error) {
					return []discovery.Peer{{
						InstanceID: "windows-one", Pairing: true, Port: 43119,
						Addresses: []net.IP{net.ParseIP("192.168.1.20"), net.ParseIP("10.0.0.20")},
					}}, nil
				},
				bootstrap: func(_ context.Context, endpoint string, _ ed25519.PublicKey) (pairing.SessionDescriptor, error) {
					endpoints = append(endpoints, endpoint)
					if strings.Contains(endpoint, "10.0.0.20") {
						return pairing.SessionDescriptor{}, errors.New("unreachable WSL interface")
					}
					return pairing.SessionDescriptor{ID: "session-ok"}, nil
				},
			}

			target, descriptor, err := transport.Bootstrap(context.Background(), selector, make(ed25519.PublicKey, ed25519.PublicKeySize))
			if err != nil {
				t.Fatalf("Bootstrap() error = %v", err)
			}
			if target.Address != "192.168.1.20" || descriptor.ID != "session-ok" {
				t.Fatalf("Bootstrap() = (%#v, %#v), want successful reachable address", target, descriptor)
			}
			want := []string{"https://10.0.0.20:43119", "https://192.168.1.20:43119"}
			if !reflect.DeepEqual(endpoints, want) {
				t.Fatalf("bootstrap endpoints = %#v, want deterministic retries %#v", endpoints, want)
			}
		})
	}
}

func TestDiscoveryPairingBootstrapIPSelectorTriesOnlySelectedAddress(t *testing.T) {
	var endpoints []string
	transport := discoveryPairingTransport{
		discover: func(context.Context) ([]discovery.Peer, error) {
			return []discovery.Peer{{
				InstanceID: "windows-one", Pairing: true, Port: 43119,
				Addresses: []net.IP{net.ParseIP("10.0.0.20"), net.ParseIP("192.168.1.20")},
			}}, nil
		},
		bootstrap: func(_ context.Context, endpoint string, _ ed25519.PublicKey) (pairing.SessionDescriptor, error) {
			endpoints = append(endpoints, endpoint)
			return pairing.SessionDescriptor{ID: "selected"}, nil
		},
	}

	target, _, err := transport.Bootstrap(context.Background(), "192.168.1.20", make(ed25519.PublicKey, ed25519.PublicKeySize))
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if target.Address != "192.168.1.20" || !reflect.DeepEqual(endpoints, []string{"https://192.168.1.20:43119"}) {
		t.Fatalf("target=%#v endpoints=%#v, want only selected IP", target, endpoints)
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
		bootstrap: func(context.Context, string, ed25519.PublicKey) (pairing.SessionDescriptor, error) {
			bootstrapCalls++
			return pairing.SessionDescriptor{}, nil
		},
	}

	_, _, err := transport.Bootstrap(context.Background(), "", make(ed25519.PublicKey, ed25519.PublicKeySize))
	if err == nil || !strings.Contains(err.Error(), "multiple pairing peers") {
		t.Fatalf("Bootstrap() error = %v, want distinct-instance ambiguity", err)
	}
	if bootstrapCalls != 0 {
		t.Fatalf("bootstrap calls = %d, want 0 for ambiguous instances", bootstrapCalls)
	}
}
