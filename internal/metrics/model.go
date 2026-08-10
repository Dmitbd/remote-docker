package metrics

import "time"

type Metric[T any] struct {
	Available bool   `json:"available"`
	Value     T      `json:"value,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func Available[T any](value T) Metric[T] {
	return Metric[T]{Available: true, Value: value}
}

func Unavailable[T any](reason string) Metric[T] {
	return Metric[T]{Reason: reason}
}

type ProcessUsage struct {
	CPUPercent  Metric[float64] `json:"cpu_percent"`
	MemoryBytes Metric[uint64]  `json:"memory_bytes"`
	Processes   int             `json:"processes,omitempty"`
}

type Rate struct {
	Available      bool    `json:"available"`
	BytesPerSecond float64 `json:"bytes_per_second,omitempty"`
	Reason         string  `json:"reason,omitempty"`
}

type RunningState struct {
	Available bool   `json:"available"`
	Running   bool   `json:"running"`
	Reason    string `json:"reason,omitempty"`
}

type Sample struct {
	At                  time.Time      `json:"at"`
	MacRemoteDocker     ProcessUsage   `json:"mac_remote_docker"`
	WindowsRemoteDocker ProcessUsage   `json:"windows_remote_docker"`
	WindowsManagedWSL   ProcessUsage   `json:"windows_managed_wsl"`
	DockerContainers    Metric[int]    `json:"docker_containers"`
	SyncNetwork         Rate           `json:"sync_network"`
	ManagedDiskBytes    Metric[uint64] `json:"managed_disk_bytes"`
	LocalDockerEngine   RunningState   `json:"local_docker_engine"`
}

type RemoteSample struct {
	WindowsRemoteDocker ProcessUsage   `json:"windows_remote_docker"`
	WindowsManagedWSL   ProcessUsage   `json:"windows_managed_wsl"`
	DockerContainers    Metric[int]    `json:"docker_containers"`
	ManagedDiskBytes    Metric[uint64] `json:"managed_disk_bytes"`
	SyncNetworkTotal    Metric[uint64] `json:"sync_network_total"`
}
