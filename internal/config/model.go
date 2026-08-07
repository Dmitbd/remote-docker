package config

const CurrentSchemaVersion = 1

// Config contains non-secret application settings.
type Config struct {
	SchemaVersion int                  `json:"schemaVersion"`
	ActiveDevice  string               `json:"activeDevice,omitempty"`
	Devices       map[string]Device    `json:"devices,omitempty"`
	Workspaces    map[string]Workspace `json:"workspaces,omitempty"`
}

// Device describes a paired remote host without storing its credentials.
type Device struct {
	Name              string `json:"name"`
	Address           string `json:"address"`
	SSHPort           int    `json:"sshPort"`
	SyncPort          int    `json:"syncPort,omitempty"`
	SSHHostPublicKey  string `json:"sshHostPublicKey,omitempty"`
	SyncthingDeviceID string `json:"syncthingDeviceId,omitempty"`
	ClientDeviceID    string `json:"clientDeviceId,omitempty"`
}

// Workspace is the persisted public registration used by bind-mount policy.
type Workspace struct {
	Path string `json:"path"`
}
