package tunnel

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

type protocolFaultConn struct {
	net.Conn
	delay      time.Duration
	resetAfter int
	written    int
}

func (c *protocolFaultConn) Write(payload []byte) (int, error) {
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	if c.resetAfter >= 0 && c.written >= c.resetAfter {
		_ = c.Conn.Close()
		return 0, net.ErrClosed
	}
	if c.resetAfter >= 0 && c.written+len(payload) > c.resetAfter {
		payload = payload[:c.resetAfter-c.written]
	}
	count, err := c.Conn.Write(payload)
	c.written += count
	if err == nil && count == 0 {
		err = net.ErrClosed
	}
	return count, err
}

func TestProtocolFaultConnectionPassDelayAndResetBoundaries(t *testing.T) {
	for _, test := range []struct {
		name       string
		delay      time.Duration
		resetAfter int
		wantError  bool
	}{
		{name: "pass", resetAfter: -1},
		{name: "delayed", delay: time.Millisecond, resetAfter: -1},
		{name: "reset first write", resetAfter: 0, wantError: true},
		{name: "reset midstream", resetAfter: 2, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			left, right := net.Pipe()
			connection := &protocolFaultConn{Conn: left, delay: test.delay, resetAfter: test.resetAfter}
			go func() { _, _ = io.Copy(io.Discard, right); _ = right.Close() }()
			_, firstErr := connection.Write([]byte("abcd"))
			_, secondErr := connection.Write([]byte("efgh"))
			_ = connection.Close()
			failed := errors.Is(firstErr, net.ErrClosed) || errors.Is(secondErr, net.ErrClosed)
			if failed != test.wantError {
				t.Fatalf("fault result first=%v second=%v, wantError=%v", firstErr, secondErr, test.wantError)
			}
		})
	}
}
