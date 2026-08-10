package workspace

// Workspace registers one local source tree that may be synchronized.
type Workspace struct {
	ID        string `json:"id"`
	LocalRoot string `json:"local_root"`
}

// PathMode determines whether a resolved path creates a sync relationship.
type PathMode string

const (
	PathModeWorkspace  PathMode = "workspace"
	PathModeRemoteOnly PathMode = "remote_only"
)

// ResolvedPath is a trusted bind source after policy evaluation.
type ResolvedPath struct {
	Local       string   `json:"local,omitempty"`
	Remote      string   `json:"remote"`
	WorkspaceID string   `json:"workspace_id,omitempty"`
	Mode        PathMode `json:"mode"`
}

// RequiresSync is false for paths that exist only in the managed runtime.
func (p ResolvedPath) RequiresSync() bool {
	return p.Mode == PathModeWorkspace
}
