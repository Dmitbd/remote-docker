package tunnel

import (
	"errors"
	"fmt"
	"io"
)

const (
	TunnelALPN  = "remote-docker-tunnel/1"
	PairingALPN = "http/1.1"
)

type StreamKind byte

const (
	StreamDockerSSH     StreamKind = 1
	StreamWorkspaceSync StreamKind = 2
	StreamControl       StreamKind = 3
	StreamMetrics       StreamKind = 4
)

var streamMagic = [4]byte{'R', 'D', 'T', '1'}

func (kind StreamKind) String() string {
	switch kind {
	case StreamDockerSSH:
		return "docker-ssh"
	case StreamWorkspaceSync:
		return "workspace-sync"
	case StreamControl:
		return "control"
	case StreamMetrics:
		return "metrics"
	default:
		return "unknown"
	}
}

func validStreamKind(kind StreamKind) bool {
	return kind >= StreamDockerSSH && kind <= StreamMetrics
}

func writeStreamHeader(writer io.Writer, kind StreamKind) error {
	if !validStreamKind(kind) {
		return fmt.Errorf("unsupported tunnel stream kind %d", kind)
	}
	header := [5]byte{streamMagic[0], streamMagic[1], streamMagic[2], streamMagic[3], byte(kind)}
	for written := 0; written < len(header); {
		count, err := writer.Write(header[written:])
		written += count
		if err != nil {
			return fmt.Errorf("write tunnel stream header: %w", err)
		}
		if count == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func readStreamHeader(reader io.Reader) (StreamKind, error) {
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, fmt.Errorf("read tunnel stream header: %w", err)
	}
	if header[0] != streamMagic[0] || header[1] != streamMagic[1] ||
		header[2] != streamMagic[2] || header[3] != streamMagic[3] {
		return 0, errors.New("invalid tunnel stream header")
	}
	kind := StreamKind(header[4])
	if !validStreamKind(kind) {
		return 0, fmt.Errorf("unsupported tunnel stream kind %d", kind)
	}
	return kind, nil
}
