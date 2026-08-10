package config

const CurrentSchemaVersion = 4

// Config contains non-secret application settings.
type Config struct {
	SchemaVersion          int                          `json:"schemaVersion"`
	ActiveDevice           string                       `json:"activeDevice,omitempty"`
	LocalSyncthingDeviceID string                       `json:"localSyncthingDeviceId,omitempty"`
	LocalSyncthingIdentity []byte                       `json:"localSyncthingIdentity,omitempty"`
	Devices                map[string]Device            `json:"devices,omitempty"`
	PendingRevocations     map[string]PendingRevocation `json:"pendingRevocations,omitempty"`
	Workspaces             map[string]Workspace         `json:"workspaces,omitempty"`
}

// Device describes a paired remote host without storing its credentials.
type Device struct {
	Name                      string              `json:"name"`
	Address                   string              `json:"address"`
	SSHPort                   int                 `json:"sshPort"`
	SyncPort                  int                 `json:"syncPort,omitempty"`
	SSHHostPublicKey          string              `json:"sshHostPublicKey,omitempty"`
	SyncthingDeviceID         string              `json:"syncthingDeviceId,omitempty"`
	ClientDeviceID            string              `json:"clientDeviceId,omitempty"`
	TunnelPort                int                 `json:"tunnelPort,omitempty"`
	TunnelPeerPublicKey       string              `json:"tunnelPeerPublicKey,omitempty"`
	TransportVersion          int                 `json:"transportVersion,omitempty"`
	RevocationProofHash       string              `json:"revocationProofHash,omitempty"`
	RevocationCredentialOwner string              `json:"revocationCredentialOwner,omitempty"`
	DockerContext             DockerContextChange `json:"dockerContext,omitempty"`
}

// DockerContextChange is the durable ownership record for the managed Docker
// context mutation made while pairing a device.
type DockerContextChange struct {
	Name           string `json:"name,omitempty"`
	PreviousHost   string `json:"previousHost,omitempty"`
	CurrentHost    string `json:"currentHost,omitempty"`
	Created        bool   `json:"created,omitempty"`
	RemoveOnUnpair bool   `json:"removeOnUnpair,omitempty"`
}

// PendingRevocation keeps only the public cleanup metadata for a device that
// is no longer trusted locally but still needs a best-effort remote revoke.
type PendingRevocation struct {
	Device           Device              `json:"device"`
	DockerContext    DockerContextChange `json:"dockerContext,omitempty"`
	SessionID        string              `json:"sessionId,omitempty"`
	LocalDeviceID    string              `json:"localDeviceId,omitempty"`
	CleanupRequested bool                `json:"cleanupRequested,omitempty"`
}

// Workspace is the persisted public registration used by bind-mount policy.
type Workspace struct {
	Path string `json:"path"`
}
