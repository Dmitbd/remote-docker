//go:build windows

package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createNoWindow = 0x08000000

type windowsSampler struct {
	mu           sync.Mutex
	previousCPU  time.Duration
	previousTime time.Time
	remote       RemoteSampler
}

func newPlatformSampler(int) PlatformSampler { return &windowsSampler{remote: windowsWSLRemote{}} }

func (s *windowsSampler) Sample(ctx context.Context, at time.Time, active bool) PlatformSample {
	result := PlatformSample{WindowsRemoteDocker: s.processUsage(at)}
	if !active {
		return result
	}
	remote, err := s.remote.SampleRemote(ctx)
	if err != nil {
		reason := "измерения managed WSL недоступны"
		remote = RemoteSample{
			WindowsManagedWSL: ProcessUsage{CPUPercent: Unavailable[float64](reason), MemoryBytes: Unavailable[uint64](reason)},
			DockerContainers:  Unavailable[int](reason), ManagedDiskBytes: Unavailable[uint64](reason),
			SyncNetworkTotal: Unavailable[uint64](reason),
		}
	}
	result.Remote = remote
	return result
}

func (s *windowsSampler) processUsage(at time.Time) ProcessUsage {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user); err != nil {
		return ProcessUsage{CPUPercent: Unavailable[float64]("CPU приложения недоступен"), MemoryBytes: windowsMemoryUsage(), Processes: 1}
	}
	cpu := time.Duration(kernel.Nanoseconds() + user.Nanoseconds())
	s.mu.Lock()
	metric := Unavailable[float64]("нужен второй измеренный образец")
	if !s.previousTime.IsZero() && at.After(s.previousTime) && cpu >= s.previousCPU {
		percent := float64(cpu-s.previousCPU) / float64(at.Sub(s.previousTime)) * 100 / float64(runtime.NumCPU())
		metric = Available(percent)
	}
	s.previousCPU = cpu
	s.previousTime = at
	s.mu.Unlock()
	return ProcessUsage{CPUPercent: metric, MemoryBytes: windowsMemoryUsage(), Processes: 1}
}

type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

func windowsMemoryUsage() Metric[uint64] {
	counters := processMemoryCounters{CB: uint32(unsafe.Sizeof(processMemoryCounters{}))}
	procedure := windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")
	result, _, _ := procedure.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&counters)), uintptr(counters.CB))
	if result == 0 {
		return Unavailable[uint64]("RAM приложения недоступна")
	}
	return Available(uint64(counters.WorkingSetSize))
}

type windowsWSLRemote struct{}

func (windowsWSLRemote) SampleRemote(ctx context.Context) (RemoteSample, error) {
	list := hiddenCommand(ctx, "wsl.exe", "--list", "--running", "--quiet")
	output, err := list.Output()
	if err != nil {
		return RemoteSample{}, err
	}
	names := strings.Split(strings.ReplaceAll(string(output), "\x00", ""), "\n")
	for index := range names {
		names[index] = strings.TrimSpace(names[index])
	}
	if !SelectManagedDistro(names, "remote-docker") {
		return RemoteSample{}, errors.New("managed WSL is stopped")
	}
	requestBody := []byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"metrics.sample\"}\n")
	command := hiddenCommand(ctx, "wsl.exe", "--distribution", "remote-docker", "--user", "root", "--exec", "/usr/local/bin/remote-docker-remote", "rpc")
	command.Stdin = bytes.NewReader(requestBody)
	responseBody, err := command.Output()
	if err != nil {
		return RemoteSample{}, err
	}
	var response struct {
		Result RemoteSample    `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil || len(response.Error) != 0 {
		return RemoteSample{}, errors.New("managed metrics response is invalid")
	}
	return response.Result, nil
}

func hiddenCommand(ctx context.Context, name string, arguments ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, name, arguments...)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return command
}

type unavailableDockerProbe struct{}

func newLocalDockerProbe() LocalDockerProbe { return unavailableDockerProbe{} }

func (unavailableDockerProbe) Running(context.Context) (bool, error) {
	return false, errors.New("local Docker check is only available on Mac")
}
