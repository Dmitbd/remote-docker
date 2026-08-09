package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteSystemObservationIncludesNarrowDockerAndSyncthingHealth(t *testing.T) {
	probes := &recordingRemoteHealthProbes{docker: true, syncthing: false}
	observation, err := (remoteSystemOperations{
		runner:         staticSystemdRunner{},
		freeBytes:      func(string) (uint64, error) { return minimumDiagnosticFreeBytes, nil },
		probes:         probes,
		presenceActive: func() bool { return true },
	}).Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if !observation.WSLRunning || !observation.SystemdTarget || !observation.DockerSocket ||
		!observation.DiskAvailable || observation.SyncthingService || !observation.PresenceActive {
		t.Fatalf("observation = %#v", observation)
	}
	if probes.dockerCalls != 1 || probes.syncthingCalls != 1 {
		t.Fatalf("probe calls docker=%d syncthing=%d, want one each", probes.dockerCalls, probes.syncthingCalls)
	}
}

func TestPresenceProcessProbeMatchesOnlyDedicatedCommand(t *testing.T) {
	for _, test := range []struct {
		command []byte
		want    bool
	}{
		{command: []byte("/usr/local/bin/remote-docker-remote\x00rpc\x00"), want: true},
		{command: []byte("/usr/local/bin/remote-docker-remote\x00health\x00"), want: false},
		{command: []byte("/tmp/remote-docker-remote-presence\x00"), want: false},
		{command: []byte("/usr/local/bin/remote-docker-remote\x00rpc\x00extra\x00"), want: false},
	} {
		if got := isDedicatedPresenceCommand(test.command); got != test.want {
			t.Fatalf("isDedicatedPresenceCommand(%q) = %v, want %v", test.command, got, test.want)
		}
	}
}

func TestPresenceMarkerRequiresLiveExactRPCProcessAndCleansUp(t *testing.T) {
	root := t.TempDir()
	marker := newProcessPresenceMarker(root, 4242)
	if err := marker.Activate(); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	if !presenceMarkerActive(root, func(pid int) bool { return pid == 4242 }) {
		t.Fatal("active presence marker was not observed")
	}
	second := newProcessPresenceMarker(root, 4343)
	second.processActive = func(pid int) bool { return pid == 4242 }
	if err := second.Activate(); err == nil {
		t.Fatal("second live presence process claimed the one-client lease")
	}
	if presenceMarkerActive(root, func(int) bool { return false }) {
		t.Fatal("stale presence marker was accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "not-a-pid"), []byte("active\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker.Deactivate()
	if presenceMarkerActive(root, func(int) bool { return true }) {
		t.Fatal("deactivated or invalid presence marker was accepted")
	}
}

func TestManagedServiceProbesUseDockerPingWithoutPublishingBody(t *testing.T) {
	secretBody := "Authorization: Bearer do-not-publish"
	var method, path string
	probes := remoteServiceProbes{dockerClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		method, path = request.Method, request.URL.Path
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(secretBody)),
			Header:     make(http.Header),
		}, nil
	})}}
	if !probes.DockerSocketHealthy(context.Background()) {
		t.Fatal("DockerSocketHealthy() = false")
	}
	if method != http.MethodGet || path != "/_ping" {
		t.Fatalf("Docker probe = %s %s, want GET /_ping", method, path)
	}
}

func TestDockerSocketDialerIgnoresRequestedNetworkAndUsesManagedUnixSocket(t *testing.T) {
	dialer := &recordingContextDialer{err: errors.New("stop after capture")}
	_, _ = (dockerSocketDialer{dialer: dialer}).DialContext(context.Background(), "tcp", "attacker.example:1234")
	if dialer.network != "unix" || dialer.address != managedDockerSocketPath {
		t.Fatalf("underlying dial = %s %s, want unix %s", dialer.network, dialer.address, managedDockerSocketPath)
	}
}

func TestManagedServiceProbesUseExactLoopbackSyncthingPort(t *testing.T) {
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	dialer := &recordingContextDialer{connection: client}
	if !(remoteServiceProbes{syncthingDialer: dialer}).SyncthingServiceHealthy(context.Background()) {
		t.Fatal("SyncthingServiceHealthy() = false")
	}
	if dialer.network != "tcp" || dialer.address != managedSyncthingHealthAddress {
		t.Fatalf("underlying dial = %s %s, want tcp %s", dialer.network, dialer.address, managedSyncthingHealthAddress)
	}
}

type staticSystemdRunner struct{}

func (staticSystemdRunner) Run(context.Context, systemdOperation) error { return nil }

type recordingRemoteHealthProbes struct {
	docker, syncthing           bool
	dockerCalls, syncthingCalls int
}

func (p *recordingRemoteHealthProbes) DockerSocketHealthy(context.Context) bool {
	p.dockerCalls++
	return p.docker
}

func (p *recordingRemoteHealthProbes) SyncthingServiceHealthy(context.Context) bool {
	p.syncthingCalls++
	return p.syncthing
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type recordingContextDialer struct {
	network, address string
	connection       net.Conn
	err              error
}

func (d *recordingContextDialer) DialContext(_ context.Context, network, address string) (net.Conn, error) {
	d.network, d.address = network, address
	return d.connection, d.err
}
