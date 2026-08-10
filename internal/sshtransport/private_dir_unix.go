//go:build darwin || linux

package sshtransport

import (
	"errors"
	"os"
	"syscall"
)

func validatePrivateDirectoryOwnership(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot verify private runtime directory owner")
	}
	return validatePrivateDirectoryOwner(int(stat.Uid), os.Geteuid())
}
