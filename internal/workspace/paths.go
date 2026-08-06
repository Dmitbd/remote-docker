package workspace

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var (
	ErrPathNotFound          = errors.New("bind source path does not exist")
	ErrOutsideWorkspace      = errors.New("bind source is outside registered workspaces")
	ErrInvalidWorkspace      = errors.New("registered workspace is invalid")
	ErrRuntimePathNotAllowed = errors.New("remote runtime path is not allowlisted")
)

// ResolveBind canonicalizes an existing local source and permits it only when
// its final target is contained by a registered workspace root.
func ResolveBind(source, cwd string, workspaces []Workspace) (ResolvedPath, error) {
	candidate, err := absoluteSource(source, cwd)
	if err != nil {
		return ResolvedPath{}, err
	}

	type registeredRoot struct {
		id        string
		canonical string
	}
	registeredRoots := make([]registeredRoot, 0, len(workspaces))
	lexicallyContained := false
	for _, registered := range workspaces {
		if strings.TrimSpace(registered.ID) == "" || strings.TrimSpace(registered.LocalRoot) == "" {
			return ResolvedPath{}, ErrInvalidWorkspace
		}
		absoluteRoot, absoluteErr := filepath.Abs(registered.LocalRoot)
		if absoluteErr != nil {
			return ResolvedPath{}, fmt.Errorf("%w: %s: %v", ErrInvalidWorkspace, registered.ID, absoluteErr)
		}
		canonicalRoot, rootErr := canonicalWorkspaceRoot(absoluteRoot)
		if rootErr != nil {
			return ResolvedPath{}, fmt.Errorf("%w: %s: %v", ErrInvalidWorkspace, registered.ID, rootErr)
		}
		registeredRoots = append(registeredRoots, registeredRoot{
			id:        registered.ID,
			canonical: canonicalRoot,
		})
		lexicallyContained = lexicallyContained || containsPath(absoluteRoot, candidate) || containsPath(canonicalRoot, candidate)
	}
	if !lexicallyContained {
		return ResolvedPath{}, fmt.Errorf("%w: %s", ErrOutsideWorkspace, candidate)
	}

	if _, err := os.Stat(candidate); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ResolvedPath{}, fmt.Errorf("%w: %s", ErrPathNotFound, candidate)
		}
		return ResolvedPath{}, fmt.Errorf("inspect bind source %s: %w", candidate, err)
	}
	canonicalSource, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ResolvedPath{}, fmt.Errorf("%w: %s", ErrPathNotFound, candidate)
		}
		return ResolvedPath{}, fmt.Errorf("resolve bind source %s: %w", candidate, err)
	}
	canonicalSource, err = filepath.Abs(canonicalSource)
	if err != nil {
		return ResolvedPath{}, fmt.Errorf("make bind source absolute: %w", err)
	}

	workspaceID := ""
	matchedRootLength := -1
	for _, registered := range registeredRoots {
		if containsPath(registered.canonical, canonicalSource) && len(registered.canonical) > matchedRootLength {
			workspaceID = registered.id
			matchedRootLength = len(registered.canonical)
		}
	}
	if workspaceID == "" {
		return ResolvedPath{}, fmt.Errorf("%w: %s", ErrOutsideWorkspace, canonicalSource)
	}

	return ResolvedPath{
		Local:       canonicalSource,
		Remote:      canonicalSource,
		WorkspaceID: workspaceID,
		Mode:        PathModeWorkspace,
	}, nil
}

func absoluteSource(source, cwd string) (string, error) {
	if strings.TrimSpace(source) == "" {
		return "", ErrPathNotFound
	}
	if filepath.IsAbs(source) {
		return filepath.Clean(source), nil
	}
	if strings.TrimSpace(cwd) == "" {
		return "", errors.New("current working directory is required for a relative bind source")
	}
	absoluteCWD, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("make current working directory absolute: %w", err)
	}
	return filepath.Abs(filepath.Join(absoluteCWD, source))
}

func canonicalWorkspaceRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("workspace root is not a directory")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Abs(canonical)
}

func containsPath(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

// RuntimeAllowlist stores exact Linux paths that exist only inside WSL.
type RuntimeAllowlist struct {
	paths map[string]struct{}
}

// NewRuntimeAllowlist validates an explicit remote-only path set.
func NewRuntimeAllowlist(paths []string) (RuntimeAllowlist, error) {
	allowlist := RuntimeAllowlist{paths: make(map[string]struct{}, len(paths))}
	for _, candidate := range paths {
		cleaned := path.Clean(candidate)
		if !path.IsAbs(candidate) || cleaned == "/" || cleaned == "." {
			return RuntimeAllowlist{}, fmt.Errorf("%w: %q", ErrRuntimePathNotAllowed, candidate)
		}
		allowlist.paths[cleaned] = struct{}{}
	}
	return allowlist, nil
}

// Resolve returns a remote-only path and never a local sync source.
func (a RuntimeAllowlist) Resolve(remote string) (ResolvedPath, error) {
	if !path.IsAbs(remote) {
		return ResolvedPath{}, fmt.Errorf("%w: %q", ErrRuntimePathNotAllowed, remote)
	}
	cleaned := path.Clean(remote)
	if _, allowed := a.paths[cleaned]; !allowed {
		return ResolvedPath{}, fmt.Errorf("%w: %q", ErrRuntimePathNotAllowed, cleaned)
	}
	return ResolvedPath{Remote: cleaned, Mode: PathModeRemoteOnly}, nil
}
