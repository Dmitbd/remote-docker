package portrelay

import "fmt"

// PortBinding is one host-side binding reported by Docker inspect.
type PortBinding struct {
	HostIP   string
	HostPort int
}

// Container is the inspect subset needed for full desired-state discovery.
type Container struct {
	ID      string
	Running bool
	Ports   map[string][]PortBinding
}

// Mapping is one loopback-only Mac relay desired for a running container.
type Mapping struct {
	Protocol    string
	LocalHost   string
	LocalPort   int
	ContainerID string
	RemoteHost  string
	RemotePort  int
}

// Key is stable across reconciles and unique per container publication.
func (m Mapping) Key() string {
	return fmt.Sprintf("%s|%s|%d|%s|%d", m.Protocol, m.LocalHost, m.LocalPort, m.ContainerID, m.RemotePort)
}

// UnsupportedMapping records a remote publication the relay refuses to expose.
type UnsupportedMapping struct {
	ContainerID string
	Protocol    string
	RemotePort  int
	Reason      string
}

// Snapshot is the complete desired state derived from all running containers.
type Snapshot struct {
	Mappings    []Mapping
	Unsupported []UnsupportedMapping
}

// Event is the Docker event subset used only as a reconcile trigger.
type Event struct {
	Type        string
	Action      string
	ContainerID string
}

// RequiresReconcile identifies container lifecycle changes.
func (e Event) RequiresReconcile() bool {
	if e.Type != "" && e.Type != "container" {
		return false
	}
	switch e.Action {
	case "create", "start", "die", "destroy":
		return true
	default:
		return false
	}
}
