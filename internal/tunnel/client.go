package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"
)

type ClientState string

const (
	ClientDisconnected ClientState = "disconnected"
	ClientConnecting   ClientState = "connecting"
	ClientConnected    ClientState = "connected"
	ClientReconnecting ClientState = "reconnecting"
)

type Client struct {
	Dial       func(context.Context) (net.Conn, error)
	NewSession func(net.Conn) (Session, error)
	OpenRelays func(Session) ([]io.Closer, error)
	OnState    func(ClientState, error)
	Wait       func(context.Context, time.Duration) bool
}

func (c *Client) Run(ctx context.Context) error {
	if c == nil || c.Dial == nil || c.OpenRelays == nil {
		return errors.New("tunnel client dependencies are incomplete")
	}
	newSession := c.NewSession
	if newSession == nil {
		newSession = NewClientSession
	}
	wait := c.Wait
	if wait == nil {
		wait = waitContext
	}
	reconnecting := false
	failures := 0
	for ctx.Err() == nil {
		if reconnecting {
			c.state(ClientReconnecting, nil)
		} else {
			c.state(ClientConnecting, nil)
		}
		connection, err := c.Dial(ctx)
		if err != nil {
			c.state(ClientDisconnected, err)
			if !wait(ctx, reconnectDelay(failures)) {
				break
			}
			failures++
			reconnecting = true
			continue
		}
		session, err := newSession(connection)
		if err != nil {
			_ = connection.Close()
			c.state(ClientDisconnected, err)
			if !wait(ctx, reconnectDelay(failures)) {
				break
			}
			failures++
			reconnecting = true
			continue
		}
		if err := waitSessionAdmission(ctx, session); err != nil {
			_ = session.Close()
			c.state(ClientDisconnected, err)
			if !wait(ctx, reconnectDelay(failures)) {
				break
			}
			failures++
			reconnecting = true
			continue
		}
		relays, err := c.OpenRelays(session)
		if err != nil {
			_ = session.Close()
			c.state(ClientDisconnected, err)
			if errors.Is(err, ErrLocalPortOccupied) {
				return err
			}
			if !wait(ctx, reconnectDelay(failures)) {
				break
			}
			failures++
			reconnecting = true
			continue
		}
		c.state(ClientConnected, nil)
		failures = 0
		select {
		case <-ctx.Done():
			closeRelays(relays)
			_ = session.Close()
			c.state(ClientDisconnected, ctx.Err())
			return ctx.Err()
		case <-session.Done():
			closeRelays(relays)
			_ = session.Close()
			c.state(ClientDisconnected, net.ErrClosed)
			reconnecting = true
			if !wait(ctx, reconnectDelay(0)) {
				return ctx.Err()
			}
		}
	}
	c.state(ClientDisconnected, ctx.Err())
	return ctx.Err()
}

func reconnectDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := 250 * time.Millisecond
	for index := 0; index < attempt && delay < 15*time.Second; index++ {
		delay *= 2
		if delay > 15*time.Second {
			delay = 15 * time.Second
		}
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return delay
	}
	unit := float64(binary.LittleEndian.Uint64(random[:])) / float64(^uint64(0))
	factor := 0.8 + unit*0.4
	return time.Duration(float64(delay) * factor)
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func closeRelays(relays []io.Closer) {
	for _, relay := range relays {
		if relay != nil {
			_ = relay.Close()
		}
	}
}

func (c *Client) state(state ClientState, err error) {
	if c.OnState != nil {
		c.OnState(state, err)
	}
}
