package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/hashicorp/yamux"
)

type Session interface {
	OpenStream(context.Context, StreamKind) (net.Conn, error)
	AcceptStream(context.Context) (StreamKind, net.Conn, error)
	Done() <-chan struct{}
	Close() error
}

type yamuxSession struct {
	session *yamux.Session
}

func NewClientSession(connection net.Conn) (Session, error) {
	if connection == nil {
		return nil, errors.New("tunnel client connection is required")
	}
	session, err := yamux.Client(connection, yamuxConfig())
	if err != nil {
		return nil, fmt.Errorf("start tunnel client session: %w", err)
	}
	return &yamuxSession{session: session}, nil
}

func NewServerSession(connection net.Conn) (Session, error) {
	if connection == nil {
		return nil, errors.New("tunnel server connection is required")
	}
	session, err := yamux.Server(connection, yamuxConfig())
	if err != nil {
		return nil, fmt.Errorf("start tunnel server session: %w", err)
	}
	return &yamuxSession{session: session}, nil
}

func yamuxConfig() *yamux.Config {
	config := yamux.DefaultConfig()
	config.AcceptBacklog = 64
	config.EnableKeepAlive = true
	config.KeepAliveInterval = 10 * time.Second
	config.StreamOpenTimeout = 5 * time.Second
	config.ConnectionWriteTimeout = 5 * time.Second
	config.LogOutput = io.Discard
	return config
}

func (s *yamuxSession) OpenStream(ctx context.Context, kind StreamKind) (net.Conn, error) {
	if !validStreamKind(kind) {
		return nil, fmt.Errorf("unsupported tunnel stream kind %d", kind)
	}
	stream, err := waitStream(ctx, s.session.OpenStream)
	if err != nil {
		return nil, fmt.Errorf("open %s tunnel stream: %w", kind, err)
	}
	if err := writeStreamHeader(stream, kind); err != nil {
		_ = stream.Close()
		return nil, err
	}
	return stream, nil
}

func (s *yamuxSession) AcceptStream(ctx context.Context) (StreamKind, net.Conn, error) {
	stream, err := waitStream(ctx, s.session.AcceptStream)
	if err != nil {
		return 0, nil, fmt.Errorf("accept tunnel stream: %w", err)
	}
	kind, err := readStreamHeader(stream)
	if err != nil {
		_ = stream.Close()
		return 0, nil, err
	}
	return kind, stream, nil
}

func waitStream(ctx context.Context, operation func() (net.Conn, error)) (net.Conn, error) {
	type result struct {
		connection net.Conn
		err        error
	}
	resultChannel := make(chan result, 1)
	go func() {
		connection, err := operation()
		resultChannel <- result{connection: connection, err: err}
	}()
	select {
	case <-ctx.Done():
		go func() {
			completed := <-resultChannel
			if completed.connection != nil {
				_ = completed.connection.Close()
			}
		}()
		return nil, ctx.Err()
	case completed := <-resultChannel:
		return completed.connection, completed.err
	}
}

func (s *yamuxSession) Done() <-chan struct{} { return s.session.CloseChan() }
func (s *yamuxSession) Close() error            { return s.session.Close() }
