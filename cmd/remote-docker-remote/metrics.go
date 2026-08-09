package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"

	"github.com/Dmitbd/remote-docker/internal/metrics"
)

func collectManagedRuntimeMetrics(ctx context.Context) metrics.RemoteSample {
	reason := "Windows не предоставляет точную привязку CPU и RAM к одной WSL-среде"
	result := metrics.RemoteSample{
		WindowsRemoteDocker: metrics.ProcessUsage{
			CPUPercent: metrics.Unavailable[float64]("Windows-приложение измеряет свой процесс локально"),
			MemoryBytes: metrics.Unavailable[uint64]("Windows-приложение измеряет свой процесс локально"),
		},
		WindowsManagedWSL: metrics.ProcessUsage{
			CPUPercent: metrics.Unavailable[float64](reason),
			MemoryBytes: metrics.Unavailable[uint64](reason),
		},
		ManagedDiskBytes: metrics.Unavailable[uint64]("точный размер managed WSL недоступен без полного сканирования диска"),
		SyncNetworkTotal: metrics.Unavailable[uint64]("сетевой трафик нельзя надёжно отделить от других процессов WSL"),
	}
	count, err := managedContainerCount(ctx)
	if err != nil {
		result.DockerContainers = metrics.Unavailable[int]("Docker Engine не вернул список контейнеров")
	} else {
		result.DockerContainers = metrics.Available(count)
	}
	return result
}

func managedContainerCount(ctx context.Context) (int, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", "/var/run/docker.sock")
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/json?all=1", nil)
	if err != nil {
		return 0, err
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, &dockerMetricsError{}
	}
	var containers []json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&containers); err != nil {
		return 0, err
	}
	return len(containers), nil
}

type dockerMetricsError struct{}

func (*dockerMetricsError) Error() string { return "Docker metrics request failed" }
