package tunnel

import (
	"bytes"
	"testing"
)

func TestStreamHeaderRoundTripForAllowList(t *testing.T) {
	for _, kind := range []StreamKind{StreamDockerSSH, StreamWorkspaceSync, StreamControl, StreamMetrics} {
		t.Run(kind.String(), func(t *testing.T) {
			var encoded bytes.Buffer
			if err := writeStreamHeader(&encoded, kind); err != nil {
				t.Fatalf("writeStreamHeader() error = %v", err)
			}
			if got := encoded.Bytes(); len(got) != 5 || string(got[:4]) != "RDT1" || got[4] != byte(kind) {
				t.Fatalf("wire header = %x", got)
			}
			decoded, err := readStreamHeader(&encoded)
			if err != nil || decoded != kind {
				t.Fatalf("readStreamHeader() = %v, %v", decoded, err)
			}
		})
	}
}

func TestStreamHeaderRejectsUnknownTruncatedAndArbitraryInput(t *testing.T) {
	for name, input := range map[string][]byte{
		"unknown":   {'R', 'D', 'T', '1', 5},
		"truncated": {'R', 'D', 'T', '1'},
		"arbitrary": {'S', 'S', 'H', '-', '2'},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readStreamHeader(bytes.NewReader(input)); err == nil {
				t.Fatalf("readStreamHeader(%x) succeeded", input)
			}
		})
	}
	if err := writeStreamHeader(&bytes.Buffer{}, 99); err == nil {
		t.Fatal("writeStreamHeader accepted unknown kind")
	}
}
