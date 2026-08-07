//go:build darwin && !cgo

package main

import (
	"context"
	"errors"
)

type nativeDirectoryPicker struct{}

func (nativeDirectoryPicker) Choose(context.Context) (string, error) {
	return "", errors.New("native folder selection requires cgo on macOS")
}
