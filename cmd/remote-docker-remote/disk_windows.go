//go:build windows

package main

import "errors"

func filesystemFreeBytes(string) (uint64, error) {
	return 0, errors.New("managed WSL filesystem is unavailable on Windows")
}
