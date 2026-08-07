//go:build windows

package main

func platformIcon(pngBytes []byte) []byte {
	return icoFromPNG(pngBytes)
}
