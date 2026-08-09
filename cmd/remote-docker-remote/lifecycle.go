package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"time"
)

var managedContainerID = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

type remoteLifecycleRuntime interface {
	StopContainers(context.Context) error
}

type managedDockerContainers struct {
	client *http.Client
}

func (m managedDockerContainers) StopContainers(ctx context.Context) error {
	client := m.client
	if client == nil {
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", managedDockerSocketPath)
		}}
		defer transport.CloseIdleConnections()
		client = &http.Client{Transport: transport, Timeout: 15 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/json?all=0", nil)
	if err != nil {
		return errors.New("create managed container list request")
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("list managed Docker containers")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return errors.New("managed Docker container list failed")
	}
	var containers []struct {
		ID string `json:"Id"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&containers); err != nil {
		return errors.New("decode managed Docker container list")
	}
	for _, container := range containers {
		if !managedContainerID.MatchString(container.ID) {
			return errors.New("managed Docker returned an invalid container ID")
		}
		endpoint := fmt.Sprintf("http://docker/containers/%s/stop?t=10", container.ID)
		stopRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
		if err != nil {
			return errors.New("create managed container stop request")
		}
		stopResponse, err := client.Do(stopRequest)
		if err != nil {
			return errors.New("stop managed Docker container")
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(stopResponse.Body, 4<<10))
		_ = stopResponse.Body.Close()
		if stopResponse.StatusCode != http.StatusNoContent && stopResponse.StatusCode != http.StatusNotModified {
			return errors.New("managed Docker container stop failed")
		}
	}
	return nil
}
