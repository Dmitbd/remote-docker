//go:build windows

package sshtransport

import "os"

func validatePrivateDirectoryOwnership(os.FileInfo) error { return nil }
