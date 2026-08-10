//go:build darwin

package metrics

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type darwinSampler struct{ rootPID int }

func newPlatformSampler(rootPID int) PlatformSampler { return darwinSampler{rootPID: rootPID} }

func (s darwinSampler) Sample(ctx context.Context, _ time.Time, _ bool) PlatformSample {
	command := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,ppid=,%cpu=,rss=")
	output, err := command.Output()
	if err != nil {
		reason := "не удалось прочитать процессы Remote Docker"
		return PlatformSample{MacRemoteDocker: ProcessUsage{
			CPUPercent: Unavailable[float64](reason), MemoryBytes: Unavailable[uint64](reason),
		}}
	}
	records := make([]ProcessRecord, 0, 8)
	reader := strings.NewReader(string(output))
	for {
		var pid, parent int
		var cpu float64
		var rssKB uint64
		if _, err := fmt.Fscan(reader, &pid, &parent, &cpu, &rssKB); err != nil {
			break
		}
		records = append(records, ProcessRecord{PID: pid, ParentPID: parent, CPUPercent: cpu, MemoryBytes: rssKB * 1024})
	}
	return PlatformSample{MacRemoteDocker: OwnedProcessUsage(records, s.rootPID)}
}

type socketDockerProbe struct{ paths []string }

func newLocalDockerProbe() LocalDockerProbe {
	paths := []string{"/var/run/docker.sock"}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".docker", "run", "docker.sock"))
	}
	return socketDockerProbe{paths: paths}
}

func (p socketDockerProbe) Running(ctx context.Context) (bool, error) {
	for _, path := range p.paths {
		dialer := net.Dialer{Timeout: 150 * time.Millisecond}
		connection, err := dialer.DialContext(ctx, "unix", path)
		if err == nil {
			_ = connection.Close()
			return true, nil
		}
	}
	return false, nil
}
