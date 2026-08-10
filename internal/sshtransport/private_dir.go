package sshtransport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// EnsurePrivateDirectory creates an app-owned directory without accepting a
// symlink, a foreign owner, or permissions that expose its contents.
func EnsurePrivateDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("private runtime directory path must be absolute")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create private runtime directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect private runtime directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("private runtime path must be a real directory")
	}
	if info.Mode().Perm() != 0o700 {
		return errors.New("private runtime directory permissions must be 0700")
	}
	if err := validatePrivateDirectoryOwnership(info); err != nil {
		return err
	}
	return nil
}

func validatePrivateDirectoryOwner(actual, expected int) error {
	if actual != expected {
		return errors.New("private runtime directory belongs to another user")
	}
	return nil
}
