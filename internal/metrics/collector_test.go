package metrics

import (
	"context"
	"testing"
	"time"
)

func TestOwnedProcessUsageIncludesOnlyRootAndDescendants(t *testing.T) {
	records := []ProcessRecord{
		{PID: 100, ParentPID: 1, CPUPercent: 2.5, MemoryBytes: 1000},
		{PID: 101, ParentPID: 100, CPUPercent: 1.5, MemoryBytes: 2000},
		{PID: 102, ParentPID: 101, CPUPercent: 0.5, MemoryBytes: 3000},
		{PID: 999, ParentPID: 1, CPUPercent: 99, MemoryBytes: 9999},
	}
	usage := OwnedProcessUsage(records, 100)
	if usage.Processes != 3 || !usage.CPUPercent.Available || usage.CPUPercent.Value != 4.5 ||
		!usage.MemoryBytes.Available || usage.MemoryBytes.Value != 6000 {
		t.Fatalf("OwnedProcessUsage() = %#v", usage)
	}
}

func TestUnavailableMetricNeverPretendsToBeZero(t *testing.T) {
	metric := Unavailable[uint64]("not attributable")
	if metric.Available || metric.Reason != "not attributable" {
		t.Fatalf("Unavailable() = %#v", metric)
	}
}

func TestRateTrackerUsesMeasuredCounterDelta(t *testing.T) {
	tracker := &RateTracker{}
	start := time.Unix(100, 0)
	if first := tracker.Update(start, Available(uint64(1000))); first.Available {
		t.Fatalf("first rate = %#v, want unavailable baseline", first)
	}
	rate := tracker.Update(start.Add(2*time.Second), Available(uint64(5000)))
	if !rate.Available || rate.BytesPerSecond != 2000 {
		t.Fatalf("second rate = %#v", rate)
	}
	reset := tracker.Update(start.Add(3*time.Second), Available(uint64(10)))
	if reset.Available {
		t.Fatalf("reset rate = %#v, want unavailable", reset)
	}
}

func TestSelectManagedDistroUsesExactName(t *testing.T) {
	if !SelectManagedDistro([]string{"Ubuntu", "remote-docker"}, "remote-docker") {
		t.Fatal("exact managed distro was not selected")
	}
	if SelectManagedDistro([]string{"remote-docker-dev", "REMOTE-DOCKER"}, "remote-docker") {
		t.Fatal("similar or differently cased distro was selected")
	}
}

func TestCollectorOnlyProbesLocalDockerState(t *testing.T) {
	probe := &recordingDockerProbe{}
	collector := NewCollector(Options{Platform: &fakePlatformSampler{}, LocalDocker: probe})
	result := collector.Sample(context.Background(), false)
	if probe.calls != 1 || !result.LocalDockerEngine.Available || result.LocalDockerEngine.Running {
		t.Fatalf("Sample() = %#v, probe calls %d", result, probe.calls)
	}
}

type fakePlatformSampler struct{}

func (*fakePlatformSampler) Sample(context.Context, time.Time, bool) PlatformSample {
	return PlatformSample{}
}

type recordingDockerProbe struct{ calls int }

func (p *recordingDockerProbe) Running(context.Context) (bool, error) {
	p.calls++
	return false, nil
}
