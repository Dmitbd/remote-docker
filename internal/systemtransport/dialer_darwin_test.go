//go:build darwin

package systemtransport

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestNetcatDialerProvidesFullDuplexConnectionAndDeadlines(t *testing.T) {
	dialer := netcatDialer{command: helperNetcatCommand}
	connection, err := dialer.DialContext(context.Background(), "tcp", "192.168.1.68:54397")
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	defer connection.Close()

	if _, err := connection.Write([]byte("pairing-data")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	response := make([]byte, len("pairing-data"))
	if _, err := io.ReadFull(connection, response); err != nil {
		t.Fatalf("ReadFull() error = %v", err)
	}
	if string(response) != "pairing-data" {
		t.Fatalf("response = %q", response)
	}

	if err := connection.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	if _, err := connection.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read() error = %v, want deadline", err)
	}
}

func TestNetcatDialerRejectsNonPrivateOrAmbiguousDestinations(t *testing.T) {
	dialer := netcatDialer{command: helperNetcatCommand}
	for _, address := range []string{"example.com:443", "8.8.8.8:443", "169.254.1.1:443", "[fe80::1]:443", "0.0.0.0:443", "192.168.1.68:0"} {
		if _, err := dialer.DialContext(context.Background(), "tcp", address); err == nil {
			t.Fatalf("DialContext(%q) succeeded", address)
		}
	}
	if _, err := dialer.DialContext(context.Background(), "udp", "192.168.1.68:54397"); err == nil {
		t.Fatal("DialContext() accepted UDP")
	}
}

func TestNetcatConnectionCloseReapsChild(t *testing.T) {
	dialer := netcatDialer{command: helperNetcatCommand}
	connection, err := dialer.DialContext(context.Background(), "tcp", "192.168.1.68:54397")
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	managed := connection.(*managedProcessConn)
	if err := connection.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case <-managed.done:
	case <-time.After(time.Second):
		t.Fatal("netcat child was not reaped")
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestNetcatDialerHonorsAlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (netcatDialer{command: helperNetcatCommand}).DialContext(ctx, "tcp", "192.168.1.68:54397"); !errors.Is(err, context.Canceled) {
		t.Fatalf("DialContext() error = %v", err)
	}
}

func TestTunnelDialerRejectsEveryPortExcept49221(t *testing.T) {
	dial := TunnelDialContext()
	if _, err := dial(context.Background(), "tcp", "192.168.1.68:49220"); err == nil {
		t.Fatal("TunnelDialContext accepted raw Syncthing port")
	}
	if _, err := dial(context.Background(), "tcp", "192.168.1.68:49222"); err == nil {
		t.Fatal("TunnelDialContext accepted raw SSH port")
	}
}

func TestNetcatHelperProcess(t *testing.T) {
	if os.Getenv("REMOTE_DOCKER_NETCAT_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(os.Stdout, os.Stdin)
	os.Exit(0)
}

func helperNetcatCommand(_ string, _ ...string) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=TestNetcatHelperProcess")
	command.Env = append(os.Environ(), "REMOTE_DOCKER_NETCAT_HELPER=1")
	return command
}

var _ net.Conn = (*managedProcessConn)(nil)
