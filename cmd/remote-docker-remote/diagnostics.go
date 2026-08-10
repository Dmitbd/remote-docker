package main

import (
	"context"
	"errors"
	"os/exec"
)

const minimumDiagnosticFreeBytes = uint64(1 << 30)

type remoteDiagnosticObservation struct {
	WSLRunning       bool `json:"wsl_running"`
	SystemdTarget    bool `json:"systemd_target"`
	DockerSocket     bool `json:"docker_socket"`
	DiskAvailable    bool `json:"disk_available"`
	SyncthingService bool `json:"syncthing_service"`
	PresenceActive   bool `json:"presence_active"`
}

type remoteDiagnosticsRuntime interface {
	Observe(context.Context) (remoteDiagnosticObservation, error)
	RestartSystemdTarget(context.Context) error
}

type systemdOperation string

const (
	systemdTargetActive  systemdOperation = "target_active"
	systemdTargetRestart systemdOperation = "target_restart"
)

type systemdRunner interface {
	Run(context.Context, systemdOperation) error
}

type remoteSystemOperations struct {
	runner         systemdRunner
	freeBytes      func(string) (uint64, error)
	probes         remoteHealthProbes
	presenceActive func() bool
}

func (o remoteSystemOperations) Observe(ctx context.Context) (remoteDiagnosticObservation, error) {
	runner := o.runner
	if runner == nil {
		runner = execSystemdRunner{}
	}
	active := runner.Run(ctx, systemdTargetActive) == nil
	readFreeBytes := o.freeBytes
	if readFreeBytes == nil {
		readFreeBytes = filesystemFreeBytes
	}
	available, err := readFreeBytes("/")
	if err != nil {
		return remoteDiagnosticObservation{}, errors.New("read managed filesystem capacity")
	}
	probes := o.probes
	if probes == nil {
		probes = remoteServiceProbes{}
	}
	presenceActive := o.presenceActive
	if presenceActive == nil {
		presenceActive = dedicatedPresenceProcessActive
	}
	return remoteDiagnosticObservation{
		WSLRunning:       true,
		SystemdTarget:    active,
		DockerSocket:     probes.DockerSocketHealthy(ctx),
		DiskAvailable:    available >= minimumDiagnosticFreeBytes,
		SyncthingService: probes.SyncthingServiceHealthy(ctx),
		PresenceActive:   presenceActive(),
	}, nil
}

func (o remoteSystemOperations) RestartSystemdTarget(ctx context.Context) error {
	runner := o.runner
	if runner == nil {
		runner = execSystemdRunner{}
	}
	if err := runner.Run(ctx, systemdTargetRestart); err != nil {
		return errors.New("restart managed systemd target")
	}
	return nil
}

func (o remoteSystemOperations) StopContainers(ctx context.Context) error {
	return (managedDockerContainers{}).StopContainers(ctx)
}

func systemdInvocation(operation systemdOperation) (string, []string, bool) {
	switch operation {
	case systemdTargetActive:
		return "/usr/bin/systemctl", []string{"is-active", "--quiet", "remote-docker.target"}, true
	case systemdTargetRestart:
		return "/usr/bin/sudo", []string{
			"--non-interactive", "/usr/bin/systemctl", "restart", "remote-docker.target",
		}, true
	default:
		return "", nil, false
	}
}

type execSystemdRunner struct{}

func (execSystemdRunner) Run(ctx context.Context, operation systemdOperation) error {
	binary, args, ok := systemdInvocation(operation)
	if !ok {
		return errors.New("unsupported managed systemd operation")
	}
	return exec.CommandContext(ctx, binary, args...).Run()
}
