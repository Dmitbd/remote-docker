package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveBindUsesCanonicalRegisteredWorkspacePaths(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "sample")
	siblingRoot := filepath.Join(root, "sample-old")
	mustMkdirAll(t, filepath.Join(workspaceRoot, "src"))
	mustMkdirAll(t, filepath.Join(workspaceRoot, "packages", "api"))
	mustMkdirAll(t, siblingRoot)
	if err := os.Symlink(siblingRoot, filepath.Join(workspaceRoot, "escape")); err != nil {
		t.Fatal(err)
	}

	workspaces := []Workspace{{ID: "sample", LocalRoot: workspaceRoot}}
	tests := []struct {
		name      string
		source    string
		cwd       string
		wantLocal string
		wantMode  PathMode
		wantErr   error
	}{
		{
			name:      "absolute descendant",
			source:    filepath.Join(workspaceRoot, "src"),
			cwd:       workspaceRoot,
			wantLocal: filepath.Join(workspaceRoot, "src"),
			wantMode:  PathModeWorkspace,
		},
		{
			name:      "relative descendant",
			source:    filepath.Join("packages", "api"),
			cwd:       workspaceRoot,
			wantLocal: filepath.Join(workspaceRoot, "packages", "api"),
			wantMode:  PathModeWorkspace,
		},
		{
			name:    "sibling prefix",
			source:  siblingRoot,
			cwd:     workspaceRoot,
			wantErr: ErrOutsideWorkspace,
		},
		{
			name:    "symlink escape",
			source:  filepath.Join(workspaceRoot, "escape"),
			cwd:     workspaceRoot,
			wantErr: ErrOutsideWorkspace,
		},
		{
			name:    "relative escape",
			source:  filepath.Join("..", "sample-old"),
			cwd:     workspaceRoot,
			wantErr: ErrOutsideWorkspace,
		},
		{
			name:    "local Docker socket",
			source:  "/var/run/docker.sock",
			cwd:     workspaceRoot,
			wantErr: ErrOutsideWorkspace,
		},
		{
			name:    "missing source",
			source:  filepath.Join(workspaceRoot, "missing"),
			cwd:     workspaceRoot,
			wantErr: ErrPathNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := ResolveBind(tt.source, tt.cwd, workspaces)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ResolveBind() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantCanonical, canonicalErr := filepath.EvalSymlinks(tt.wantLocal)
			if canonicalErr != nil {
				t.Fatal(canonicalErr)
			}
			if resolved.Local != wantCanonical || resolved.Remote != wantCanonical {
				t.Fatalf("resolved paths = local %q remote %q, want %q", resolved.Local, resolved.Remote, wantCanonical)
			}
			if resolved.WorkspaceID != "sample" || resolved.Mode != tt.wantMode || !resolved.RequiresSync() {
				t.Fatalf("ResolveBind() = %#v", resolved)
			}
		})
	}
}

func TestResolveBindChoosesMostSpecificNestedWorkspace(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	mustMkdirAll(t, nested)
	resolved, err := ResolveBind(nested, root, []Workspace{
		{ID: "root", LocalRoot: root},
		{ID: "nested", LocalRoot: nested},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.WorkspaceID != "nested" {
		t.Fatalf("WorkspaceID = %q, want nested", resolved.WorkspaceID)
	}
}

func TestResolveRuntimePathIsRemoteOnlyAndExactAllowlist(t *testing.T) {
	allowlist, err := NewRuntimeAllowlist([]string{"/var/run/docker.sock", "/run/remote-docker/control.sock"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := allowlist.Resolve("/var/run/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Local != "" || resolved.Remote != "/var/run/docker.sock" || resolved.WorkspaceID != "" || resolved.Mode != PathModeRemoteOnly {
		t.Fatalf("runtime path = %#v", resolved)
	}
	if resolved.RequiresSync() {
		t.Fatal("remote-only runtime path requested a sync folder")
	}
	if _, err := allowlist.Resolve("/var/run/docker.sock/child"); !errors.Is(err, ErrRuntimePathNotAllowed) {
		t.Fatalf("descendant error = %v, want ErrRuntimePathNotAllowed", err)
	}
	if _, err := allowlist.Resolve("relative.sock"); !errors.Is(err, ErrRuntimePathNotAllowed) {
		t.Fatalf("relative error = %v, want ErrRuntimePathNotAllowed", err)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
