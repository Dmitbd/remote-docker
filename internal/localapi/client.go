package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
)

type DialFunc func(context.Context) (net.Conn, error)

type Client struct {
	Endpoint      string
	SchemaVersion int
	Dial          DialFunc
}

var requestID atomic.Uint64

func (c Client) Call(ctx context.Context, method Method, params any, result any) error {
	if !method.valid() {
		return errors.New("invalid local control method")
	}
	dial := c.Dial
	if dial == nil {
		dial = func(ctx context.Context) (net.Conn, error) {
			return dialLocal(ctx, c.Endpoint)
		}
	}
	connection, err := dial(ctx)
	if err != nil {
		return fmt.Errorf("connect to background agent: %w", err)
	}
	defer connection.Close()
	if !isLocalNetwork(connection.RemoteAddr().Network()) {
		return ErrInsecureTransport
	}

	var rawParams json.RawMessage
	if params != nil {
		rawParams, err = json.Marshal(params)
		if err != nil {
			return fmt.Errorf("encode local control parameters: %w", err)
		}
	}
	schemaVersion := c.SchemaVersion
	if schemaVersion == 0 {
		schemaVersion = CurrentSchemaVersion
	}
	id := requestID.Add(1)
	if err := json.NewEncoder(connection).Encode(wireRequest{
		SchemaVersion: schemaVersion,
		ID:            id,
		Method:        method,
		Params:        rawParams,
	}); err != nil {
		return fmt.Errorf("write local control request: %w", err)
	}

	decoder := json.NewDecoder(io.LimitReader(connection, maxMessageSize))
	decoder.DisallowUnknownFields()
	var response wireResponse
	if err := decoder.Decode(&response); err != nil {
		return fmt.Errorf("read local control response: %w", err)
	}
	if response.SchemaVersion != CurrentSchemaVersion || response.ID != id && response.ID != 0 {
		return &RemoteError{Code: ErrorSchemaMismatch, Message: "local control API response mismatch"}
	}
	if response.Error != nil {
		return &RemoteError{Code: response.Error.Code, Message: response.Error.Message}
	}
	if result != nil && len(response.Result) > 0 && string(response.Result) != "null" {
		if err := json.Unmarshal(response.Result, result); err != nil {
			return fmt.Errorf("decode local control result: %w", err)
		}
	}
	return nil
}
