// Package watchdog provides a private crash-cleanup mode for the manually
// launched desktop executable. It is never installed as a service or login
// item and exists only while an enabled session is active.
package watchdog

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

const (
	InternalArgument = "--internal-watchdog"
	messageInit      = "init"
	messageClean     = "clean"
	tokenBytes       = 32
	tokenHexLength   = tokenBytes * 2
)

type protocolMessage struct {
	Kind  string `json:"kind"`
	Token string `json:"token"`
}

type Controller struct {
	token string
	input io.WriteCloser
	done  chan error

	signalOnce sync.Once
	signalErr  error
	waitOnce   sync.Once
	waitReady  chan struct{}
	waitErr    error
}

func Start(executable string) (*Controller, error) {
	if executable == "" {
		return nil, errors.New("watchdog executable is required")
	}
	random := make([]byte, tokenBytes)
	if _, err := io.ReadFull(rand.Reader, random); err != nil {
		return nil, errors.New("create watchdog token")
	}
	token := hex.EncodeToString(random)
	clear(random)

	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, errors.New("create watchdog control pipe")
	}
	command := exec.Command(executable, InternalArgument)
	command.Stdin = reader
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	configureChildProcess(command)
	if err := command.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, errors.New("start crash watchdog")
	}
	_ = reader.Close()
	if err := json.NewEncoder(writer).Encode(protocolMessage{Kind: messageInit, Token: token}); err != nil {
		_ = writer.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, errors.New("initialize crash watchdog")
	}
	controller := &Controller{token: token, input: writer, done: make(chan error, 1)}
	go func() {
		controller.done <- command.Wait()
		close(controller.done)
	}()
	return controller, nil
}

func (c *Controller) CleanStop(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.signalOnce.Do(func() {
		if err := json.NewEncoder(c.input).Encode(protocolMessage{Kind: messageClean, Token: c.token}); err != nil {
			c.signalErr = errors.New("notify crash watchdog")
		}
		if err := c.input.Close(); err != nil && c.signalErr == nil {
			c.signalErr = errors.New("close crash watchdog control pipe")
		}
	})
	c.waitOnce.Do(func() {
		c.waitReady = make(chan struct{})
		go func() {
			err, ok := <-c.done
			if ok {
				c.waitErr = err
			}
			close(c.waitReady)
		}()
	})
	select {
	case <-ctx.Done():
		return errors.Join(c.signalErr, ctx.Err())
	case <-c.waitReady:
		if c.waitErr != nil {
			return errors.Join(c.signalErr, fmt.Errorf("crash watchdog did not exit cleanly: %w", c.waitErr))
		}
		return c.signalErr
	}
}

// RunChild is called by the desktop main function before initializing the UI.
func RunChild(ctx context.Context, input io.Reader) int {
	return runChild(ctx, input, cleanupOwnedResources)
}

func runChild(ctx context.Context, input io.Reader, cleanup func(context.Context) error) int {
	if input == nil || cleanup == nil {
		return 2
	}
	decoder := json.NewDecoder(io.LimitReader(input, 8<<10))
	decoder.DisallowUnknownFields()
	var initialization protocolMessage
	if err := decoder.Decode(&initialization); err != nil || initialization.Kind != messageInit || !validToken(initialization.Token) {
		return 2
	}
	var terminal protocolMessage
	err := decoder.Decode(&terminal)
	clean := err == nil && terminal.Kind == messageClean && validToken(terminal.Token) &&
		subtle.ConstantTimeCompare([]byte(initialization.Token), []byte(terminal.Token)) == 1
	if clean {
		return 0
	}
	if err := cleanup(ctx); err != nil {
		return 1
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return 1
	}
	if terminal.Kind != "" {
		return 1
	}
	return 0
}

func validToken(token string) bool {
	if len(token) != tokenHexLength {
		return false
	}
	decoded, err := hex.DecodeString(token)
	clear(decoded)
	return err == nil
}

func clear(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
