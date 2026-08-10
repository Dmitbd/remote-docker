//go:build darwin

package discovery

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	bonjourBinary          = "/usr/bin/dns-sd"
	bonjourOutputLimit     = 256 << 10
	bonjourLineLimit       = 64 << 10
	bonjourCandidateLimit  = 64
	bonjourProcessWaitTime = time.Second
)

type bonjourCommandFactory func(context.Context, string, ...string) *exec.Cmd

// SystemBrowser delegates Bonjour access to Apple's system-signed dns-sd
// client. This lets an unsigned background agent avoid opening mDNS sockets.
type SystemBrowser struct {
	command bonjourCommandFactory
}

// NewBrowser creates the platform production discovery browser.
func NewBrowser() (Browser, error) {
	if info, err := os.Stat(bonjourBinary); err != nil || info.IsDir() {
		return nil, errors.New("macOS Bonjour client is unavailable")
	}
	return &SystemBrowser{command: exec.CommandContext}, nil
}

// Browse resolves matching Bonjour records until ctx expires.
func (b *SystemBrowser) Browse(ctx context.Context, serviceType string) (<-chan Record, error) {
	if b == nil || serviceType != ServiceType {
		return nil, errors.New("unsupported Bonjour service type")
	}
	command := b.command
	if command == nil {
		command = exec.CommandContext
	}
	records := make(chan Record)
	go func() {
		defer close(records)
		seen := make(map[string]struct{})
		_ = runBonjourLines(ctx, command, []string{"-B", serviceType, "local."}, func(line string) bool {
			instance, ok := parseBonjourBrowseLine(line, serviceType)
			if !ok {
				return true
			}
			if _, duplicate := seen[instance]; duplicate {
				return true
			}
			if len(seen) >= bonjourCandidateLimit {
				return false
			}
			seen[instance] = struct{}{}
			record, err := resolveBonjourRecord(ctx, command, serviceType, instance)
			if err != nil {
				return ctx.Err() == nil
			}
			select {
			case records <- record:
				return true
			case <-ctx.Done():
				return false
			}
		})
	}()
	return records, nil
}

func resolveBonjourRecord(ctx context.Context, command bonjourCommandFactory, serviceType, instance string) (Record, error) {
	var host string
	var port int
	var txt []string
	err := runBonjourLines(ctx, command, []string{"-L", instance, serviceType, "local."}, func(line string) bool {
		if parsedHost, parsedPort, ok := parseBonjourResolveLine(line); ok &&
			strings.Contains(line, instance+"."+serviceType+".local.") {
			host, port = parsedHost, parsedPort
		}
		if parsedTXT, ok := parseBonjourTXTLine(line); ok {
			txt = parsedTXT
		}
		return host == "" || len(txt) == 0
	})
	if err != nil || host == "" || !validPort(port) || len(txt) == 0 || !validBonjourHost(host) {
		return Record{}, errors.New("Bonjour service resolution failed")
	}

	var address net.IP
	err = runBonjourLines(ctx, command, []string{"-G", "v4v6", host}, func(line string) bool {
		if parsed, ok := parseBonjourAddressLine(line); ok {
			address = parsed
			return false
		}
		return true
	})
	if err != nil || address == nil {
		return Record{}, errors.New("Bonjour host address resolution failed")
	}
	return Record{Port: port, TXT: txt, Addresses: []net.IP{address}}, nil
}

func runBonjourLines(ctx context.Context, factory bonjourCommandFactory, args []string, consume func(string) bool) error {
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := factory(childCtx, bonjourBinary, args...)
	command.WaitDelay = bonjourProcessWaitTime
	stdout, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("capture Bonjour output: %w", err)
	}
	stderr := &boundedBuffer{remaining: bonjourLineLimit}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start Bonjour client: %w", err)
	}

	stopped := false
	total := 0
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), bonjourLineLimit)
	for scanner.Scan() {
		total += len(scanner.Bytes()) + 1
		if total > bonjourOutputLimit {
			cancel()
			_ = command.Wait()
			return errors.New("Bonjour output exceeds size limit")
		}
		if !consume(scanner.Text()) {
			stopped = true
			cancel()
			break
		}
	}
	scanErr := scanner.Err()
	waitErr := command.Wait()
	if scanErr != nil {
		return fmt.Errorf("read Bonjour output: %w", scanErr)
	}
	if stopped {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return fmt.Errorf("Bonjour client failed: %s", message)
		}
		return fmt.Errorf("Bonjour client failed: %w", waitErr)
	}
	return nil
}

func parseBonjourBrowseLine(line, serviceType string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) < 7 || fields[1] != "Add" || fields[4] != "local." || fields[5] != serviceType+"." {
		return "", false
	}
	instance := strings.Join(fields[6:], " ")
	const prefix = "remote-docker-"
	if !strings.HasPrefix(instance, prefix) || validateOpaqueID(strings.TrimPrefix(instance, prefix)) != nil {
		return "", false
	}
	return instance, true
}

func parseBonjourResolveLine(line string) (string, int, bool) {
	const marker = " can be reached at "
	_, target, found := strings.Cut(line, marker)
	if !found {
		return "", 0, false
	}
	target, _, _ = strings.Cut(target, " (interface ")
	host, portText, err := net.SplitHostPort(strings.TrimSpace(target))
	if err != nil {
		return "", 0, false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || !validPort(port) || !validBonjourHost(host) {
		return "", 0, false
	}
	return host, port, true
}

func parseBonjourTXTLine(line string) ([]string, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil, false
	}
	for _, field := range fields {
		if !strings.Contains(field, "=") {
			return nil, false
		}
	}
	if _, ok := parseTXT(fields); !ok {
		return nil, false
	}
	return append([]string(nil), fields...), true
}

func parseBonjourAddressLine(line string) (net.IP, bool) {
	fields := strings.Fields(line)
	if len(fields) < 7 || fields[1] != "Add" {
		return nil, false
	}
	value := fields[len(fields)-2]
	if zone := strings.IndexByte(value, '%'); zone >= 0 {
		value = value[:zone]
	}
	address := net.ParseIP(value)
	if !isSystemLocalAddress(address) {
		return nil, false
	}
	return address, true
}

func isSystemLocalAddress(address net.IP) bool {
	return address != nil && !address.IsUnspecified() && !address.IsLinkLocalUnicast() &&
		(address.IsPrivate() || address.IsLoopback())
}

func validBonjourHost(host string) bool {
	if len(host) == 0 || len(host) > 253 || strings.HasPrefix(host, "-") || !strings.HasSuffix(host, ".") {
		return false
	}
	for _, character := range strings.TrimSuffix(host, ".") {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return true
}

type boundedBuffer struct {
	buffer    bytes.Buffer
	remaining int
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	if len(value) > b.remaining {
		value = value[:b.remaining]
	}
	b.remaining -= len(value)
	_, _ = b.buffer.Write(value)
	return original, nil
}

func (b *boundedBuffer) String() string { return b.buffer.String() }

var _ Browser = (*SystemBrowser)(nil)
