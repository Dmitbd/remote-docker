//go:build !windows

package main

import "syscall"

func filesystemFreeBytes(path string) (uint64, error) {
	var filesystem syscall.Statfs_t
	if err := syscall.Statfs(path, &filesystem); err != nil {
		return 0, err
	}
	if filesystem.Bavail <= 0 || filesystem.Bsize <= 0 {
		return 0, nil
	}
	return uint64(filesystem.Bavail) * uint64(filesystem.Bsize), nil
}
