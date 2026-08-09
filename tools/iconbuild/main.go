package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

var appSizes = []int{16, 20, 24, 32, 48, 64, 128, 256}
var trayStates = []string{"paused", "search", "pairing", "connected", "error"}

func main() {
	root, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	sourcePath := filepath.Join(root, "assets", "icon", "source", "remote-docker-master.png")
	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		panic(err)
	}
	source, err := png.Decode(sourceFile)
	_ = sourceFile.Close()
	if err != nil {
		panic(err)
	}
	app1024 := nearest(source, 1024)
	internalData := filepath.Join(root, "internal", "assets", "data")
	if err := os.MkdirAll(internalData, 0o755); err != nil {
		panic(err)
	}
	mustWritePNG(filepath.Join(root, "assets", "icon", "app", "remote-docker-1024.png"), app1024)
	mustWritePNG(filepath.Join(internalData, "app.png"), app1024)
	mustWriteICO(filepath.Join(root, "assets", "icon", "app", "remote-docker.ico"), app1024)
	mustWriteICNS(filepath.Join(root, "assets", "icon", "app", "remote-docker.icns"), app1024)
	for _, state := range trayStates {
		for _, size := range []int{16, 32} {
			name := state + ".png"
			if size == 32 {
				name = state + "@2x.png"
			}
			icon := trayIcon(state, size, true)
			mustWritePNG(filepath.Join(root, "assets", "icon", "tray", "darwin", name), icon)
			if size == 32 {
				mustWritePNG(filepath.Join(internalData, "tray-darwin-"+state+".png"), icon)
			}
		}
		for _, size := range []int{16, 20, 24, 32} {
			icon := trayIcon(state, size, false)
			mustWritePNG(filepath.Join(root, "assets", "icon", "tray", "windows", fmt.Sprintf("%s-%d.png", state, size)), icon)
			if size == 32 {
				mustWritePNG(filepath.Join(internalData, "tray-windows-"+state+".png"), icon)
			}
		}
	}
}

func nearest(source image.Image, size int) *image.NRGBA {
	bounds := source.Bounds()
	result := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		sy := bounds.Min.Y + y*bounds.Dy()/size
		for x := 0; x < size; x++ {
			sx := bounds.Min.X + x*bounds.Dx()/size
			result.Set(x, y, source.At(sx, sy))
		}
	}
	return result
}

func trayIcon(state string, size int, monochrome bool) *image.NRGBA {
	base := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	portal := color.NRGBA{R: 30, G: 218, B: 236, A: 255}
	accent := color.NRGBA{R: 92, G: 84, B: 255, A: 255}
	marker := color.NRGBA{R: 255, G: 175, B: 29, A: 255}
	if monochrome {
		portal = color.NRGBA{A: 255}
		accent = portal
		marker = portal
	}
	for _, point := range [][2]int{
		{6, 1}, {7, 1}, {8, 1}, {9, 1}, {5, 2}, {10, 2}, {4, 3}, {11, 3},
		{3, 4}, {12, 4}, {3, 5}, {12, 5}, {2, 6}, {13, 6}, {2, 7}, {13, 7},
		{2, 8}, {13, 8}, {2, 9}, {13, 9}, {3, 10}, {12, 10}, {3, 11}, {12, 11},
		{4, 12}, {11, 12}, {5, 13}, {10, 13}, {6, 14}, {7, 14}, {8, 14}, {9, 14},
	} {
		base.SetNRGBA(point[0], point[1], portal)
	}
	for _, point := range [][2]int{{6, 2}, {9, 2}, {4, 4}, {11, 4}, {3, 7}, {12, 7}, {4, 11}, {11, 11}, {6, 13}, {9, 13}} {
		base.SetNRGBA(point[0], point[1], accent)
	}
	switch state {
	case "paused":
		for x := 6; x <= 9; x++ {
			base.SetNRGBA(x, 8, marker)
		}
	case "search":
		base.SetNRGBA(6, 7, marker)
		base.SetNRGBA(8, 7, marker)
		base.SetNRGBA(10, 7, marker)
	case "pairing":
		for _, point := range [][2]int{{6, 7}, {7, 7}, {9, 7}, {10, 7}, {7, 8}, {9, 8}} {
			base.SetNRGBA(point[0], point[1], marker)
		}
	case "connected":
		for _, point := range [][2]int{{5, 8}, {6, 9}, {7, 10}, {8, 9}, {9, 8}, {10, 7}} {
			base.SetNRGBA(point[0], point[1], marker)
		}
	case "error":
		for y := 5; y <= 9; y++ {
			base.SetNRGBA(8, y, marker)
		}
		base.SetNRGBA(8, 11, marker)
	}
	if size == 16 {
		return base
	}
	return nearest(base, size)
}

func mustWritePNG(path string, value image.Image) {
	file, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	if err := png.Encode(file, value); err != nil {
		_ = file.Close()
		panic(err)
	}
	if err := file.Close(); err != nil {
		panic(err)
	}
}

func encodedPNG(value image.Image) []byte {
	var output bytes.Buffer
	if err := png.Encode(&output, value); err != nil {
		panic(err)
	}
	return output.Bytes()
}

func mustWriteICO(path string, source image.Image) {
	images := make([][]byte, 0, len(appSizes))
	for _, size := range appSizes {
		images = append(images, encodedPNG(nearest(source, size)))
	}
	var output bytes.Buffer
	_ = binary.Write(&output, binary.LittleEndian, uint16(0))
	_ = binary.Write(&output, binary.LittleEndian, uint16(1))
	_ = binary.Write(&output, binary.LittleEndian, uint16(len(images)))
	offset := 6 + len(images)*16
	for index, data := range images {
		size := appSizes[index]
		width := byte(size)
		if size == 256 {
			width = 0
		}
		output.WriteByte(width)
		output.WriteByte(width)
		output.WriteByte(0)
		output.WriteByte(0)
		_ = binary.Write(&output, binary.LittleEndian, uint16(1))
		_ = binary.Write(&output, binary.LittleEndian, uint16(32))
		_ = binary.Write(&output, binary.LittleEndian, uint32(len(data)))
		_ = binary.Write(&output, binary.LittleEndian, uint32(offset))
		offset += len(data)
	}
	for _, data := range images {
		output.Write(data)
	}
	if err := os.WriteFile(path, output.Bytes(), 0o644); err != nil {
		panic(err)
	}
}

func mustWriteICNS(path string, source image.Image) {
	types := []struct {
		name string
		size int
	}{{"ic10", 1024}, {"ic09", 512}, {"ic08", 256}, {"ic07", 128}, {"icp6", 64}, {"icp5", 32}, {"icp4", 16}}
	var body bytes.Buffer
	for _, item := range types {
		data := encodedPNG(nearest(source, item.size))
		body.WriteString(item.name)
		_ = binary.Write(&body, binary.BigEndian, uint32(len(data)+8))
		body.Write(data)
	}
	var output bytes.Buffer
	output.WriteString("icns")
	_ = binary.Write(&output, binary.BigEndian, uint32(body.Len()+8))
	output.Write(body.Bytes())
	if err := os.WriteFile(path, output.Bytes(), 0o644); err != nil {
		panic(err)
	}
}
