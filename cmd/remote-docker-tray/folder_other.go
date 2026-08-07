//go:build !darwin && !windows

package main

import (
	"context"
	"errors"
)

type nativeDirectoryPicker struct{}

func (nativeDirectoryPicker) Choose(context.Context) (string, error) {
	return "", errors.New("native folder selection is unsupported on this platform")
}
