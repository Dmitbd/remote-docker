package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"time"
)

const (
	managedDockerSocketPath       = "/var/run/docker.sock"
	managedSyncthingHealthAddress = "127.0.0.1:8384"
	remoteHealthProbeTimeout      = 2 * time.Second
)

type remoteHealthProbes interface {
	DockerSocketHealthy(context.Context) bool
	SyncthingServiceHealthy(context.Context) bool
}

type contextDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

type remoteServiceProbes struct {
	dockerClient    *http.Client
	syncthingDialer contextDialer
}

type dockerSocketDialer struct {
	dialer contextDialer
}

func (d dockerSocketDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	dialer := d.dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	return dialer.DialContext(ctx, "unix", managedDockerSocketPath)
}

func (p remoteServiceProbes) DockerSocketHealthy(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, remoteHealthProbeTimeout)
	defer cancel()

	client := p.dockerClient
	if client == nil {
		transport := &http.Transport{DialContext: (dockerSocketDialer{}).DialContext}
		defer transport.CloseIdleConnections()
		client = &http.Client{Transport: transport}
	}
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		return false
	}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	return response.StatusCode == http.StatusOK
}

func (p remoteServiceProbes) SyncthingServiceHealthy(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, remoteHealthProbeTimeout)
	defer cancel()

	dialer := p.syncthingDialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	connection, err := dialer.DialContext(probeCtx, "tcp", managedSyncthingHealthAddress)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}
