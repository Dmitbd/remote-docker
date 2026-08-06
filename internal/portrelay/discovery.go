package portrelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DecodeInspect reads the Docker inspect JSON subset without trusting event state.
func DecodeInspect(reader io.Reader) ([]Container, error) {
	var inspected []struct {
		ID    string `json:"Id"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		NetworkSettings struct {
			Ports map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			} `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	decoder := json.NewDecoder(io.LimitReader(reader, 32<<20))
	if err := decoder.Decode(&inspected); err != nil {
		return nil, fmt.Errorf("decode Docker inspect: %w", err)
	}
	containers := make([]Container, 0, len(inspected))
	for _, item := range inspected {
		container := Container{ID: item.ID, Running: item.State.Running, Ports: make(map[string][]PortBinding)}
		for port, bindings := range item.NetworkSettings.Ports {
			for _, binding := range bindings {
				hostPort, err := strconv.Atoi(binding.HostPort)
				if err != nil || hostPort < 1 || hostPort > 65535 {
					continue
				}
				container.Ports[port] = append(container.Ports[port], PortBinding{HostIP: binding.HostIP, HostPort: hostPort})
			}
		}
		containers = append(containers, container)
	}
	return containers, nil
}

// DecodeEvent reads one newline-delimited Docker event.
func DecodeEvent(data []byte) (Event, error) {
	var raw struct {
		Type   string `json:"Type"`
		Action string `json:"Action"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Event{}, fmt.Errorf("decode Docker event: %w", err)
	}
	return Event{Type: raw.Type, Action: raw.Action, ContainerID: raw.ID}, nil
}

// Discover derives deterministic desired state from every running container.
func Discover(containers []Container) Snapshot {
	byKey := make(map[string]Mapping)
	unsupportedByKey := make(map[string]UnsupportedMapping)
	for _, container := range containers {
		if !container.Running {
			continue
		}
		for exposed, bindings := range container.Ports {
			remotePort, protocol, ok := parseExposedPort(exposed)
			if !ok {
				continue
			}
			for _, binding := range bindings {
				if protocol != "tcp" {
					unsupported := UnsupportedMapping{
						ContainerID: container.ID,
						Protocol:    protocol,
						RemotePort:  remotePort,
						Reason:      "only TCP publications can be relayed",
					}
					key := fmt.Sprintf("%s|%s|%d", container.ID, protocol, remotePort)
					unsupportedByKey[key] = unsupported
					continue
				}
				mapping := Mapping{
					Protocol: "tcp", LocalHost: "127.0.0.1", LocalPort: binding.HostPort,
					ContainerID: container.ID, RemoteHost: binding.HostIP, RemotePort: remotePort,
				}
				byKey[mapping.Key()] = mapping
			}
		}
	}

	snapshot := Snapshot{
		Mappings:    make([]Mapping, 0, len(byKey)),
		Unsupported: make([]UnsupportedMapping, 0, len(unsupportedByKey)),
	}
	for _, mapping := range byKey {
		snapshot.Mappings = append(snapshot.Mappings, mapping)
	}
	for _, unsupported := range unsupportedByKey {
		snapshot.Unsupported = append(snapshot.Unsupported, unsupported)
	}
	sort.Slice(snapshot.Mappings, func(i, j int) bool { return snapshot.Mappings[i].Key() < snapshot.Mappings[j].Key() })
	sort.Slice(snapshot.Unsupported, func(i, j int) bool {
		left, right := snapshot.Unsupported[i], snapshot.Unsupported[j]
		return fmt.Sprintf("%s|%s|%d", left.ContainerID, left.Protocol, left.RemotePort) <
			fmt.Sprintf("%s|%s|%d", right.ContainerID, right.Protocol, right.RemotePort)
	})
	return snapshot
}

func parseExposedPort(value string) (int, string, bool) {
	portText, protocol, found := strings.Cut(value, "/")
	if !found || protocol == "" {
		return 0, "", false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return 0, "", false
	}
	return port, strings.ToLower(protocol), true
}

// Source returns complete running state and restartable event streams.
type Source interface {
	RunningContainers(context.Context) ([]Container, error)
	Events(context.Context) (<-chan Event, error)
}

// Sink applies one complete desired relay snapshot.
type Sink interface {
	Apply(context.Context, Snapshot) error
}

// Reconciler treats events as hints and always recomputes full desired state.
type Reconciler struct {
	Source     Source
	Sink       Sink
	MinBackoff time.Duration
	MaxBackoff time.Duration
	Sleep      func(context.Context, time.Duration) error
}

// Reconcile applies one snapshot of all running containers.
func (r Reconciler) Reconcile(ctx context.Context) error {
	if r.Source == nil || r.Sink == nil {
		return errors.New("port relay reconciler dependencies are incomplete")
	}
	containers, err := r.Source.RunningContainers(ctx)
	if err != nil {
		return fmt.Errorf("list running containers: %w", err)
	}
	if err := r.Sink.Apply(ctx, Discover(containers)); err != nil {
		return fmt.Errorf("apply port relay snapshot: %w", err)
	}
	return nil
}

// HandleEvent performs a full reconcile for relevant lifecycle events.
func (r Reconciler) HandleEvent(ctx context.Context, event Event) error {
	if !event.RequiresReconcile() {
		return nil
	}
	return r.Reconcile(ctx)
}

// Run reconnects the Docker event stream forever with bounded exponential backoff.
func (r Reconciler) Run(ctx context.Context) error {
	minimum := r.MinBackoff
	if minimum <= 0 {
		minimum = 250 * time.Millisecond
	}
	maximum := r.MaxBackoff
	if maximum < minimum {
		maximum = 5 * time.Second
	}
	sleep := r.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	backoff := minimum
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.Reconcile(ctx); err == nil {
			stream, streamErr := r.Source.Events(ctx)
			if streamErr == nil {
				received := false
				for event := range stream {
					received = true
					if err := r.HandleEvent(ctx, event); err != nil {
						break
					}
				}
				if received {
					backoff = minimum
				}
			}
		}
		if err := sleep(ctx, backoff); err != nil {
			return err
		}
		backoff *= 2
		if backoff > maximum {
			backoff = maximum
		}
	}
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
