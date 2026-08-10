package metrics

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"
)

type ProcessRecord struct {
	PID         int
	ParentPID   int
	CPUPercent  float64
	MemoryBytes uint64
}

func OwnedProcessUsage(records []ProcessRecord, rootPID int) ProcessUsage {
	owned := map[int]bool{rootPID: true}
	changed := true
	for changed {
		changed = false
		for _, record := range records {
			if !owned[record.PID] && owned[record.ParentPID] {
				owned[record.PID] = true
				changed = true
			}
		}
	}
	usage := ProcessUsage{CPUPercent: Available(0.0), MemoryBytes: Available(uint64(0))}
	for _, record := range records {
		if !owned[record.PID] {
			continue
		}
		usage.Processes++
		usage.CPUPercent.Value += record.CPUPercent
		usage.MemoryBytes.Value += record.MemoryBytes
	}
	if usage.Processes == 0 {
		return ProcessUsage{
			CPUPercent:  Unavailable[float64]("процессы Remote Docker не найдены"),
			MemoryBytes: Unavailable[uint64]("процессы Remote Docker не найдены"),
		}
	}
	return usage
}

func SelectManagedDistro(names []string, managed string) bool {
	for _, name := range names {
		if name == managed {
			return true
		}
	}
	return false
}

type RateTracker struct {
	mu       sync.Mutex
	previous Metric[uint64]
	at       time.Time
}

func (t *RateTracker) Update(at time.Time, counter Metric[uint64]) Rate {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !counter.Available {
		t.previous = counter
		t.at = at
		return Rate{Reason: counter.Reason}
	}
	if !t.previous.Available || t.at.IsZero() || !at.After(t.at) || counter.Value < t.previous.Value {
		t.previous = counter
		t.at = at
		return Rate{Reason: "нужен второй измеренный образец"}
	}
	seconds := at.Sub(t.at).Seconds()
	rate := float64(counter.Value-t.previous.Value) / seconds
	t.previous = counter
	t.at = at
	return Rate{Available: true, BytesPerSecond: rate}
}

type PlatformSample struct {
	MacRemoteDocker     ProcessUsage
	WindowsRemoteDocker ProcessUsage
	Remote              RemoteSample
}

type PlatformSampler interface {
	Sample(context.Context, time.Time, bool) PlatformSample
}

type RemoteSampler interface {
	SampleRemote(context.Context) (RemoteSample, error)
}

type LocalDockerProbe interface {
	Running(context.Context) (bool, error)
}

type Options struct {
	Platform    PlatformSampler
	Remote      RemoteSampler
	LocalDocker LocalDockerProbe
	Now         func() time.Time
}

type Collector struct {
	platform    PlatformSampler
	remote      RemoteSampler
	localDocker LocalDockerProbe
	now         func() time.Time
	rate        RateTracker
}

func NewCollector(options Options) *Collector {
	platform := options.Platform
	if platform == nil {
		platform = newPlatformSampler(os.Getpid())
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	localDocker := options.LocalDocker
	if localDocker == nil {
		localDocker = newLocalDockerProbe()
	}
	return &Collector{platform: platform, remote: options.Remote, localDocker: localDocker, now: now}
}

func (c *Collector) Sample(ctx context.Context, active bool) Sample {
	if ctx == nil {
		ctx = context.Background()
	}
	at := c.now()
	result := Sample{At: at}
	if c.platform != nil {
		platform := c.platform.Sample(ctx, at, active)
		result.MacRemoteDocker = platform.MacRemoteDocker
		result.WindowsRemoteDocker = platform.WindowsRemoteDocker
		if hasRemoteSample(platform.Remote) {
			applyRemote(&result, platform.Remote, &c.rate, at)
		}
	}
	if active && c.remote != nil {
		remote, err := c.remote.SampleRemote(ctx)
		if err == nil {
			applyRemote(&result, remote, &c.rate, at)
		} else {
			setRemoteUnavailable(&result, errors.New("Windows не вернул измерения"))
		}
	} else if !active && !result.DockerContainers.Available {
		setRemoteUnavailable(&result, errors.New("соединение не активно"))
	}
	if c.localDocker != nil {
		running, err := c.localDocker.Running(ctx)
		if err == nil {
			result.LocalDockerEngine = RunningState{Available: true, Running: running}
		} else {
			result.LocalDockerEngine = RunningState{Reason: "состояние локального Docker недоступно"}
		}
	}
	return result
}

func hasRemoteSample(sample RemoteSample) bool {
	return sample.WindowsRemoteDocker.CPUPercent.Available || sample.WindowsRemoteDocker.CPUPercent.Reason != "" ||
		sample.WindowsRemoteDocker.MemoryBytes.Available || sample.WindowsRemoteDocker.MemoryBytes.Reason != "" ||
		sample.WindowsManagedWSL.CPUPercent.Available || sample.WindowsManagedWSL.CPUPercent.Reason != "" ||
		sample.WindowsManagedWSL.MemoryBytes.Available || sample.WindowsManagedWSL.MemoryBytes.Reason != "" ||
		sample.DockerContainers.Available || sample.DockerContainers.Reason != "" ||
		sample.ManagedDiskBytes.Available || sample.ManagedDiskBytes.Reason != "" ||
		sample.SyncNetworkTotal.Available || sample.SyncNetworkTotal.Reason != ""
}

func applyRemote(result *Sample, remote RemoteSample, tracker *RateTracker, at time.Time) {
	if !result.WindowsRemoteDocker.CPUPercent.Available && result.WindowsRemoteDocker.CPUPercent.Reason == "" &&
		!result.WindowsRemoteDocker.MemoryBytes.Available && result.WindowsRemoteDocker.MemoryBytes.Reason == "" {
		result.WindowsRemoteDocker = remote.WindowsRemoteDocker
	}
	result.WindowsManagedWSL = remote.WindowsManagedWSL
	result.DockerContainers = remote.DockerContainers
	result.ManagedDiskBytes = remote.ManagedDiskBytes
	result.SyncNetwork = tracker.Update(at, remote.SyncNetworkTotal)
}

func setRemoteUnavailable(result *Sample, err error) {
	reason := err.Error()
	if !result.WindowsRemoteDocker.CPUPercent.Available && !result.WindowsRemoteDocker.MemoryBytes.Available {
		result.WindowsRemoteDocker = ProcessUsage{
			CPUPercent: Unavailable[float64](reason), MemoryBytes: Unavailable[uint64](reason),
		}
	}
	result.WindowsManagedWSL = ProcessUsage{
		CPUPercent: Unavailable[float64](reason), MemoryBytes: Unavailable[uint64](reason),
	}
	result.DockerContainers = Unavailable[int](reason)
	result.ManagedDiskBytes = Unavailable[uint64](reason)
	result.SyncNetwork = Rate{Reason: reason}
}
