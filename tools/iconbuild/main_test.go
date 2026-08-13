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
	"sort"
	"testing"
)

var trayICOSizes = []int{16, 20, 24, 32}

type icoEntry struct {
	width    int
	height   int
	planes   uint16
	bitCount uint16
	length   uint32
	offset   uint32
	image    image.Image
}

func TestGeneratorBuildsDeterministicTrayICOAssets(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	markerMasks := make(map[string]string, len(trayStates))
	for _, state := range trayStates {
		images := make([]image.Image, 0, len(traySizes))
		for _, size := range traySizes {
			images = append(images, trayIcon(state, size, false))
		}
		first := encodeICO(images, traySizes)
		second := encodeICO(images, traySizes)
		if !bytes.Equal(first, second) {
			t.Fatalf("tray ICO %q changed between consecutive generator runs", state)
		}
		path := filepath.Join(root, "internal", "assets", "data", "tray-windows-"+state+".ico")
		checkedIn, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read generated tray ICO %q: %v", state, err)
		}
		if !bytes.Equal(first, checkedIn) {
			t.Fatalf("checked-in tray ICO %q does not match deterministic generator bytes", state)
		}
		entries := parseICO(t, checkedIn)
		assertTrayICOContract(t, state, checkedIn, entries)
		markerMasks[state] = markerMask(t, state, entries[0].image)
	}

	uniqueMasks := make(map[string]string, len(markerMasks))
	for state, mask := range markerMasks {
		if previous, ok := uniqueMasks[mask]; ok {
			t.Fatalf("states %q and %q have identical multi-pixel markers", previous, state)
		}
		uniqueMasks[mask] = state
	}
}

func TestGenericICOEncodingPreservesApplicationICO(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	sourceFile, err := os.Open(filepath.Join(root, "assets", "icon", "source", "remote-docker-master.png"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := png.Decode(sourceFile)
	_ = sourceFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	app1024 := nearest(source, 1024)
	images := make([]image.Image, 0, len(appSizes))
	for _, size := range appSizes {
		images = append(images, nearest(app1024, size))
	}
	actual := encodeICO(images, appSizes)
	expected, err := os.ReadFile(filepath.Join(root, "assets", "icon", "app", "remote-docker.ico"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("generic ICO encoder changed the application ICO bytes")
	}
}

func TestTrayPNGSourcesRemainGeneratedFromTheSelectedPixelSign(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	for _, state := range trayStates {
		for _, size := range traySizes {
			assertPNGBytes(t, encodedPNG(trayIcon(state, size, false)), filepath.Join(root, "assets", "icon", "tray", "windows", fmt.Sprintf("%s-%d.png", state, size)))
		}
		for _, size := range []int{16, 32} {
			name := state + ".png"
			if size == 32 {
				name = state + "@2x.png"
			}
			expected := encodedPNG(trayIcon(state, size, true))
			assertPNGBytes(t, expected, filepath.Join(root, "assets", "icon", "tray", "darwin", name))
			if size == 32 {
				assertPNGBytes(t, expected, filepath.Join(root, "internal", "assets", "data", "tray-darwin-"+state+".png"))
			}
		}
	}
}

func assertPNGBytes(t *testing.T, expected []byte, path string) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Errorf("generated PNG differs from selected source asset %q", path)
	}
}

func parseICO(t *testing.T, data []byte) []icoEntry {
	t.Helper()
	if len(data) < 6 {
		t.Fatalf("ICO is shorter than its header: %d bytes", len(data))
	}
	reserved := binary.LittleEndian.Uint16(data[0:2])
	iconType := binary.LittleEndian.Uint16(data[2:4])
	count := int(binary.LittleEndian.Uint16(data[4:6]))
	if reserved != 0 || iconType != 1 || count != len(trayICOSizes) {
		t.Fatalf("invalid ICO header: reserved=%d type=%d count=%d", reserved, iconType, count)
	}
	if len(data) < 6+count*16 {
		t.Fatalf("ICO is shorter than its directory: %d bytes", len(data))
	}
	entries := make([]icoEntry, 0, count)
	for index := 0; index < count; index++ {
		start := 6 + index*16
		width := int(data[start])
		height := int(data[start+1])
		if width == 0 {
			width = 256
		}
		if height == 0 {
			height = 256
		}
		length := binary.LittleEndian.Uint32(data[start+8 : start+12])
		offset := binary.LittleEndian.Uint32(data[start+12 : start+16])
		end := uint64(offset) + uint64(length)
		if length == 0 || uint64(offset) < uint64(6+count*16) || end > uint64(len(data)) {
			t.Fatalf("ICO entry %d is out of bounds: offset=%d length=%d file=%d", index, offset, length, len(data))
		}
		decoded, format, err := image.Decode(bytes.NewReader(data[offset:uint32(end)]))
		if err != nil {
			t.Fatalf("decode ICO entry %d: %v", index, err)
		}
		if format != "png" {
			t.Fatalf("ICO entry %d is %q, want PNG", index, format)
		}
		entries = append(entries, icoEntry{
			width:    width,
			height:   height,
			planes:   binary.LittleEndian.Uint16(data[start+4 : start+6]),
			bitCount: binary.LittleEndian.Uint16(data[start+6 : start+8]),
			length:   length,
			offset:   offset,
			image:    decoded,
		})
	}
	return entries
}

func assertTrayICOContract(t *testing.T, state string, data []byte, entries []icoEntry) {
	t.Helper()
	previousEnd := uint64(6 + len(entries)*16)
	for index, entry := range entries {
		expectedSize := trayICOSizes[index]
		if entry.width != expectedSize || entry.height != expectedSize {
			t.Errorf("%s entry %d dimensions = %dx%d, want %dx%d", state, index, entry.width, entry.height, expectedSize, expectedSize)
		}
		if entry.planes != 1 || entry.bitCount != 32 {
			t.Errorf("%s entry %d metadata = planes %d, bitcount %d; want 1/32", state, index, entry.planes, entry.bitCount)
		}
		if bounds := entry.image.Bounds(); bounds.Dx() != expectedSize || bounds.Dy() != expectedSize {
			t.Errorf("%s entry %d decoded dimensions = %dx%d, want %dx%d", state, index, bounds.Dx(), bounds.Dy(), expectedSize, expectedSize)
		}
		if uint64(entry.offset) < previousEnd {
			t.Errorf("%s entry %d overlaps the previous entry", state, index)
		}
		previousEnd = uint64(entry.offset) + uint64(entry.length)
		if opaquePixelCount(entry.image) < expectedSize {
			t.Errorf("%s entry %d has no meaningful opaque silhouette", state, index)
		}
		if markerPixelCount(entry.image) < 3 {
			t.Errorf("%s entry %d marker uses fewer than three pixels", state, index)
		}
	}
	if previousEnd > uint64(len(data)) {
		t.Fatalf("%s entries exceed ICO size", state)
	}
}

func opaquePixelCount(value image.Image) int {
	count := 0
	for y := value.Bounds().Min.Y; y < value.Bounds().Max.Y; y++ {
		for x := value.Bounds().Min.X; x < value.Bounds().Max.X; x++ {
			_, _, _, alpha := value.At(x, y).RGBA()
			if alpha == 0xffff {
				count++
			}
		}
	}
	return count
}

func markerPixelCount(value image.Image) int {
	marker := color.NRGBA{R: 255, G: 175, B: 29, A: 255}
	count := 0
	for y := value.Bounds().Min.Y; y < value.Bounds().Max.Y; y++ {
		for x := value.Bounds().Min.X; x < value.Bounds().Max.X; x++ {
			if color.NRGBAModel.Convert(value.At(x, y)).(color.NRGBA) == marker {
				count++
			}
		}
	}
	return count
}

func markerMask(t *testing.T, state string, value image.Image) string {
	t.Helper()
	marker := color.NRGBA{R: 255, G: 175, B: 29, A: 255}
	points := make([]string, 0)
	for y := value.Bounds().Min.Y; y < value.Bounds().Max.Y; y++ {
		for x := value.Bounds().Min.X; x < value.Bounds().Max.X; x++ {
			if color.NRGBAModel.Convert(value.At(x, y)).(color.NRGBA) == marker {
				points = append(points, fmt.Sprintf("%d,%d", x, y))
			}
		}
	}
	if len(points) < 3 {
		t.Fatalf("state %q has only %d marker pixels", state, len(points))
	}
	sort.Strings(points)
	return fmt.Sprint(points)
}
