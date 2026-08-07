package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
)

const maxMessageSize = 1 << 20

var (
	ErrInsecureTransport = errors.New("local control API accepts only Unix sockets or Windows named pipes")
	ErrPeerOwnership     = errors.New("local control API peer is owned by another user")
)

type PeerAuthorizer func(net.Conn) error

type Server struct {
	Handler       Handler
	AuthorizePeer PeerAuthorizer
}

func (s Server) Serve(ctx context.Context, listener net.Listener) error {
	if listener == nil {
		return errors.New("local control API listener is nil")
	}
	if !isLocalNetwork(listener.Addr().Network()) {
		return ErrInsecureTransport
	}

	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stop:
		}
	}()
	defer close(stop)

	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept local control connection: %w", err)
		}
		go func() {
			_ = s.ServeConn(ctx, connection)
		}()
	}
}

func (s Server) ServeConn(ctx context.Context, connection net.Conn) error {
	if connection == nil {
		return errors.New("local control connection is nil")
	}
	defer connection.Close()
	if !isLocalNetwork(connection.LocalAddr().Network()) {
		return ErrInsecureTransport
	}

	authorize := s.AuthorizePeer
	if authorize == nil {
		authorize = authorizeCurrentUser
	}
	if err := authorize(connection); err != nil {
		return writeWireResponse(connection, wireResponse{
			SchemaVersion: CurrentSchemaVersion,
			Error:         &wireError{Code: ErrorPeerForbidden, Message: "local control peer is not the current user"},
		})
	}

	decoder := json.NewDecoder(io.LimitReader(connection, maxMessageSize))
	decoder.DisallowUnknownFields()
	var request wireRequest
	if err := decoder.Decode(&request); err != nil {
		return writeWireResponse(connection, wireResponse{
			SchemaVersion: CurrentSchemaVersion,
			Error:         &wireError{Code: ErrorInvalidRequest, Message: "invalid local control request"},
		})
	}
	response := wireResponse{SchemaVersion: CurrentSchemaVersion, ID: request.ID}
	if request.SchemaVersion != CurrentSchemaVersion {
		response.Error = &wireError{Code: ErrorSchemaMismatch, Message: "local control API schema version mismatch"}
		return writeWireResponse(connection, response)
	}
	if !request.Method.valid() {
		response.Error = &wireError{Code: ErrorInvalidRequest, Message: "unknown local control method"}
		return writeWireResponse(connection, response)
	}
	if s.Handler == nil {
		response.Error = &wireError{Code: ErrorUnavailable, Message: "background agent is unavailable"}
		return writeWireResponse(connection, response)
	}

	result, err := s.Handler.Handle(ctx, request.Method, request.Params)
	if err != nil {
		var public *PublicError
		if errors.As(err, &public) && public.Code != "" && public.Message != "" {
			response.Error = &wireError{Code: public.Code, Message: public.Message}
		} else {
			response.Error = &wireError{Code: ErrorInternal, Message: "background agent operation failed"}
		}
		return writeWireResponse(connection, response)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		response.Error = &wireError{Code: ErrorInternal, Message: "background agent operation failed"}
	} else {
		response.Result = raw
	}
	return writeWireResponse(connection, response)
}

func writeWireResponse(writer io.Writer, response wireResponse) error {
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		return fmt.Errorf("write local control response: %w", err)
	}
	return nil
}

func isLocalNetwork(network string) bool {
	switch network {
	case "unix", "unixpacket", "pipe":
		return true
	default:
		return false
	}
}
