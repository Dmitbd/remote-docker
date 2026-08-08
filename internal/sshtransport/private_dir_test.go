package sshtransport

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsurePrivateDirectoryCreatesExactPrivateDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime")

	if err := EnsurePrivateDirectory(path); err != nil {
		t.Fatalf("EnsurePrivateDirectory() error = %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v, want private directory 0700", info.Mode())
	}
}

func TestEnsurePrivateDirectoryRejectsSymlinkAndLoosePermissions(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDirectory(link); err == nil {
		t.Fatal("EnsurePrivateDirectory(symlink) error = nil")
	}

	loose := filepath.Join(root, "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDirectory(loose); err == nil {
		t.Fatal("EnsurePrivateDirectory(loose permissions) error = nil")
	}
}

func TestValidatePrivateDirectoryOwnerRejectsForeignOwner(t *testing.T) {
	if err := validatePrivateDirectoryOwner(501, 502); err == nil {
		t.Fatal("validatePrivateDirectoryOwner(foreign owner) error = nil")
	}
}
