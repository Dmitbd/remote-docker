//go:build !darwin && !windows

package main

func platformIcon(pngBytes []byte) []byte {
	return pngBytes
}
