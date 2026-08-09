package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

const managedPresenceMarkerRoot = "/var/lib/remote-docker/presence"

type rpcPresenceMarker interface {
	Activate() error
	Deactivate()
}

type processPresenceMarker struct {
	root          string
	pid           int
	processActive func(int) bool
}

func newProcessPresenceMarker(root string, pid int) *processPresenceMarker {
	return &processPresenceMarker{root: root, pid: pid, processActive: linuxPresenceProcessActive}
}

func (m *processPresenceMarker) Activate() error {
	if m == nil || m.root == "" || m.pid <= 0 {
		return errors.New("presence process marker is invalid")
	}
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return errors.New("create presence marker directory")
	}
	if err := os.Chmod(m.root, 0o700); err != nil {
		return errors.New("secure presence marker directory")
	}
	path := filepath.Join(m.root, "active")
	for attempt := 0; attempt < 2; attempt++ {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, writeErr := file.WriteString(strconv.Itoa(m.pid) + "\n")
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				_ = os.Remove(path)
				return errors.New("write presence process marker")
			}
			return nil
		}
		if !errors.Is(err, os.ErrExist) {
			return errors.New("create presence process marker")
		}
		pid, readErr := readPresenceMarkerPID(path)
		processActive := m.processActive
		if processActive == nil {
			processActive = linuxPresenceProcessActive
		}
		if readErr == nil && pid != m.pid && processActive(pid) {
			return errors.New("another presence process is active")
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.New("remove stale presence process marker")
		}
	}
	return errors.New("claim presence process marker")
}

func (m *processPresenceMarker) Deactivate() {
	if m == nil || m.root == "" || m.pid <= 0 {
		return
	}
	path := filepath.Join(m.root, "active")
	pid, err := readPresenceMarkerPID(path)
	if err == nil && pid == m.pid {
		_ = os.Remove(path)
	}
}

func dedicatedPresenceProcessActive() bool {
	return presenceMarkerActive(managedPresenceMarkerRoot, linuxPresenceProcessActive)
}

func presenceMarkerActive(root string, processActive func(int) bool) bool {
	if processActive == nil {
		return false
	}
	pid, err := readPresenceMarkerPID(filepath.Join(root, "active"))
	return err == nil && processActive(pid)
}

func readPresenceMarkerPID(path string) (int, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(string(bytes.TrimSpace(value)))
	if err != nil || pid <= 0 {
		return 0, errors.New("presence process marker is invalid")
	}
	return pid, nil
}

func linuxPresenceProcessActive(pid int) bool {
	command, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	return err == nil && isDedicatedPresenceCommand(command)
}

func isDedicatedPresenceCommand(command []byte) bool {
	parts := bytes.Split(bytes.TrimSuffix(command, []byte{0}), []byte{0})
	return len(parts) == 2 && string(parts[0]) == "/usr/local/bin/remote-docker-remote" && string(parts[1]) == "rpc"
}
