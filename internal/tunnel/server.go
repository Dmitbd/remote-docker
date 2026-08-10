package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
)

type ServiceDialer interface {
	DialService(context.Context, StreamKind) (net.Conn, error)
}

type ServerState string

const (
	ServerWaiting ServerState = "waiting"
	ServerConnected ServerState = "connected"
	ServerBusy ServerState = "busy"
)

type Server struct {
	Accept  func(context.Context) (Session, error)
	Dialer  ServiceDialer
	OnState func(ServerState)
}

func (s *Server) Run(ctx context.Context) error {
	if s == nil || s.Accept == nil || s.Dialer == nil {
		return errors.New("tunnel server dependencies are incomplete")
	}
	type accepted struct {
		session Session
		err     error
	}
	acceptedSessions := make(chan accepted)
	acceptCtx, cancelAccept := context.WithCancel(ctx)
	defer cancelAccept()
	go func() {
		for acceptCtx.Err() == nil {
			session, err := s.Accept(acceptCtx)
			select {
			case acceptedSessions <- accepted{session: session, err: err}:
			case <-acceptCtx.Done():
				if session != nil {
					_ = session.Close()
				}
				return
			}
			if err != nil {
				return
			}
		}
	}()
	s.state(ServerWaiting)
	var active Session
	var activeCancel context.CancelFunc
	activeDone := make(chan error, 1)
	for {
		select {
		case <-ctx.Done():
			if activeCancel != nil {
				activeCancel()
			}
			if active != nil {
				_ = active.Close()
				<-activeDone
			}
			return ctx.Err()
		case result := <-acceptedSessions:
			if result.err != nil {
				if ctx.Err() != nil || errors.Is(result.err, net.ErrClosed) {
					continue
				}
				return result.err
			}
			if active != nil {
				s.state(ServerBusy)
				_ = result.session.Close()
				s.state(ServerConnected)
				continue
			}
			active = result.session
			var sessionCtx context.Context
			sessionCtx, activeCancel = context.WithCancel(ctx)
			s.state(ServerConnected)
			go func(session Session) { activeDone <- s.serveSession(sessionCtx, session) }(active)
		case <-activeDone:
			active = nil
			if activeCancel != nil {
				activeCancel()
				activeCancel = nil
			}
			s.state(ServerWaiting)
		}
	}
}

func (s *Server) serveSession(ctx context.Context, session Session) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	var streams sync.WaitGroup
	defer func() {
		cancel()
		_ = session.Close()
		streams.Wait()
	}()
	go func() {
		select {
		case <-session.Done():
			cancel()
		case <-sessionCtx.Done():
		}
	}()
	for {
		kind, downstream, err := session.AcceptStream(sessionCtx)
		if err != nil {
			return err
		}
		if !validStreamKind(kind) {
			_ = downstream.Close()
			continue
		}
		upstream, err := s.Dialer.DialService(sessionCtx, kind)
		if err != nil {
			_ = downstream.Close()
			continue
		}
		streams.Add(1)
		go func() {
			defer streams.Done()
			joinConnections(sessionCtx, downstream, upstream)
		}()
	}
}

func joinConnections(ctx context.Context, first, second net.Conn) {
	defer first.Close()
	defer second.Close()
	done := make(chan struct{}, 2)
	copyOneWay := func(destination, source net.Conn) {
		_, _ = io.Copy(destination, source)
		if writer, ok := destination.(interface{ CloseWrite() error }); ok {
			_ = writer.CloseWrite()
		}
		done <- struct{}{}
	}
	go copyOneWay(first, second)
	go copyOneWay(second, first)
	select {
	case <-ctx.Done():
		_ = first.Close()
		_ = second.Close()
		<-done
		<-done
	case <-done:
		<-done
	}
}

func (s *Server) state(state ServerState) {
	if s.OnState != nil {
		s.OnState(state)
	}
}
