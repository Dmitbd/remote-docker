//go:build darwin

package app

import (
	"os"
	"path/filepath"
	"strconv"
)

func defaultPrivateRuntimeRoot() string {
	return filepath.Join("/private/tmp", "remote-docker-"+strconv.Itoa(os.Geteuid()))
}
