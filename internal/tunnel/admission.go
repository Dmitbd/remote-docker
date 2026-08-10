package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const admissionFrameTimeout = 2 * time.Second

type admissionStatus byte

const (
	admissionAccepted admissionStatus = 1
	admissionPeerBusy admissionStatus = 2
)

var (
	admissionMagic = [4]byte{'R', 'D', 'A', '1'}
	// ErrPeerBusy is returned only after the authenticated Windows peer
	// explicitly rejects this Mac because another client owns the session.
	ErrPeerBusy = errors.New("paired host already has an active client")
)

type admissionSession interface {
	Session
	sendAdmission(context.Context, admissionStatus) error
	waitAdmission(context.Context) error
}

func sendSessionAdmission(ctx context.Context, session Session, status admissionStatus) error {
	admission, ok := session.(admissionSession)
	if !ok {
		return nil
	}
	return admission.sendAdmission(ctx, status)
}

func waitSessionAdmission(ctx context.Context, session Session) error {
	admission, ok := session.(admissionSession)
	if !ok {
		return nil
	}
	return admission.waitAdmission(ctx)
}

func (s *yamuxSession) sendAdmission(ctx context.Context, status admissionStatus) error {
	if status != admissionAccepted && status != admissionPeerBusy {
		return errors.New("invalid tunnel admission status")
	}
	stream, err := waitStream(ctx, func() (net.Conn, error) {
		return s.session.OpenStream()
	})
	if err != nil {
		return fmt.Errorf("open tunnel admission stream: %w", err)
	}
	defer stream.Close()
	header := [5]byte{admissionMagic[0], admissionMagic[1], admissionMagic[2], admissionMagic[3], byte(status)}
	return withAdmissionDeadline(ctx, stream, func() error {
		for written := 0; written < len(header); {
			count, writeErr := stream.Write(header[written:])
			written += count
			if writeErr != nil {
				return fmt.Errorf("write tunnel admission: %w", writeErr)
			}
			if count == 0 {
				return io.ErrShortWrite
			}
		}
		return nil
	})
}

func (s *yamuxSession) waitAdmission(ctx context.Context) error {
	stream, err := waitStream(ctx, func() (net.Conn, error) {
		return s.session.AcceptStream()
	})
	if err != nil {
		return fmt.Errorf("accept tunnel admission stream: %w", err)
	}
	defer stream.Close()
	var header [5]byte
	if err := withAdmissionDeadline(ctx, stream, func() error {
		if _, readErr := io.ReadFull(stream, header[:]); readErr != nil {
			return fmt.Errorf("read tunnel admission: %w", readErr)
		}
		return nil
	}); err != nil {
		return err
	}
	if header[0] != admissionMagic[0] || header[1] != admissionMagic[1] ||
		header[2] != admissionMagic[2] || header[3] != admissionMagic[3] {
		return errors.New("invalid tunnel admission frame")
	}
	switch admissionStatus(header[4]) {
	case admissionAccepted:
		return nil
	case admissionPeerBusy:
		return ErrPeerBusy
	default:
		return errors.New("invalid tunnel admission status")
	}
}

func withAdmissionDeadline(ctx context.Context, connection net.Conn, operation func() error) error {
	deadline := time.Now().Add(admissionFrameTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return fmt.Errorf("set tunnel admission deadline: %w", err)
	}
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			_ = connection.SetDeadline(time.Now())
		case <-stopWatch:
		}
	}()
	err := operation()
	close(stopWatch)
	<-watchDone
	if clearErr := connection.SetDeadline(time.Time{}); err == nil && clearErr != nil {
		err = fmt.Errorf("clear tunnel admission deadline: %w", clearErr)
	}
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
