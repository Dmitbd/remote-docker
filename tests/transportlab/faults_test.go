package transportlab_test

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

type faultMode int

const (
	faultPass faultMode = iota
	faultDelayed
	faultResetFirstWrite
	faultResetMidstream
)

type faultConn struct {
	net.Conn
	mode    faultMode
	written int
}

func (c *faultConn) Write(payload []byte) (int, error) {
	switch c.mode {
	case faultDelayed:
		time.Sleep(10 * time.Millisecond)
	case faultResetFirstWrite:
		_ = c.Conn.Close()
		return 0, net.ErrClosed
	case faultResetMidstream:
		if c.written > 0 {
			_ = c.Conn.Close()
			return 0, net.ErrClosed
		}
	}
	count, err := c.Conn.Write(payload)
	c.written += count
	return count, err
}

func TestTransportFilterModesAreDeterministic(t *testing.T) {
	for _, mode := range []faultMode{faultPass, faultDelayed, faultResetFirstWrite, faultResetMidstream} {
		left, right := net.Pipe()
		filtered := &faultConn{Conn: left, mode: mode}
		read := make(chan []byte, 2)
		go func() {
			payload, _ := io.ReadAll(right)
			read <- payload
		}()
		firstCount, firstErr := filtered.Write([]byte("first"))
		secondCount, secondErr := filtered.Write([]byte("second"))
		_ = filtered.Close()
		_ = right.Close()
		switch mode {
		case faultPass, faultDelayed:
			if firstErr != nil || secondErr != nil || firstCount+secondCount != 11 {
				t.Fatalf("mode %d writes = %d/%v %d/%v", mode, firstCount, firstErr, secondCount, secondErr)
			}
		case faultResetFirstWrite:
			if !errors.Is(firstErr, net.ErrClosed) || firstCount != 0 {
				t.Fatalf("reset-first write = %d, %v", firstCount, firstErr)
			}
		case faultResetMidstream:
			if firstErr != nil || firstCount != 5 || !errors.Is(secondErr, net.ErrClosed) || secondCount != 0 {
				t.Fatalf("reset-midstream writes = %d/%v %d/%v", firstCount, firstErr, secondCount, secondErr)
			}
		}
	}
}
