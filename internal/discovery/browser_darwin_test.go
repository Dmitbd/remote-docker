//go:build darwin

package discovery

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

func TestParseBonjourBrowseLine(t *testing.T) {
	line := "21:14:22.262  Add        2  11 local.               _remote-docker._tcp. remote-docker-GNGZHCRUORSISNQHIEUT6H7KZQ"
	instance, ok := parseBonjourBrowseLine(line, ServiceType)
	if !ok || instance != "remote-docker-GNGZHCRUORSISNQHIEUT6H7KZQ" {
		t.Fatalf("parseBonjourBrowseLine() = %q, %t", instance, ok)
	}
	for _, malformed := range []string{
		"21:14:22.262  Rmv        2  11 local. _remote-docker._tcp. remote-docker-one",
		"21:14:22.262  Add        2  11 example. _remote-docker._tcp. remote-docker-one",
		"21:14:22.262  Add        2  11 local. _other._tcp. remote-docker-one",
	} {
		if _, accepted := parseBonjourBrowseLine(malformed, ServiceType); accepted {
			t.Fatalf("accepted malformed browse line %q", malformed)
		}
	}
}

func TestParseBonjourResolveOutput(t *testing.T) {
	line := "21:15:39.351  remote-docker-GNGZHCRUORSISNQHIEUT6H7KZQ._remote-docker._tcp.local. can be reached at DESKTOP-15A3GMV.local.:54397 (interface 11)"
	host, port, ok := parseBonjourResolveLine(line)
	if !ok || host != "DESKTOP-15A3GMV.local." || port != 54397 {
		t.Fatalf("parseBonjourResolveLine() = %q, %d, %t", host, port, ok)
	}
	txt, ok := parseBonjourTXTLine(" version=1 instance=GNGZHCRUORSISNQHIEUT6H7KZQ pairing=1")
	if !ok || !reflect.DeepEqual(txt, []string{"version=1", "instance=GNGZHCRUORSISNQHIEUT6H7KZQ", "pairing=1"}) {
		t.Fatalf("parseBonjourTXTLine() = %#v, %t", txt, ok)
	}
}

func TestParseBonjourAddressAcceptsOnlyPrivateOrLoopback(t *testing.T) {
	lines := []struct {
		line string
		want string
	}{
		{"21:16:00.544  Add  40000003  11  host.local.  2A02:2168:B602:1300:2075:F75D:86B6:1365%<0>  120", ""},
		{"21:16:00.544  Add  40000003  11  host.local.  FE80:0000:0000:0000:2740:FDA6:011C:1038%en0  120", ""},
		{"21:16:00.544  Add  40000003  11  host.local.  192.168.1.68  120", "192.168.1.68"},
		{"21:16:00.544  Add  40000003  11  host.local.  172.21.112.1  120", "172.21.112.1"},
		{"21:16:00.544  Add  40000003  11  host.local.  127.0.0.1  120", "127.0.0.1"},
	}
	for _, item := range lines {
		address, ok := parseBonjourAddressLine(item.line)
		if item.want == "" {
			if ok {
				t.Fatalf("accepted %q as %s", item.line, address)
			}
			continue
		}
		if !ok || address.String() != item.want {
			t.Fatalf("parseBonjourAddressLine(%q) = %v, %t", item.line, address, ok)
		}
	}
}

func TestSystemBrowserDiscoversObservedWindowsAdvertisement(t *testing.T) {
	browser := &SystemBrowser{command: helperDNSCommand}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	records, err := browser.Browse(ctx, ServiceType)
	if err != nil {
		t.Fatalf("Browse() error = %v", err)
	}
	select {
	case record := <-records:
		if record.Port != 54397 || !reflect.DeepEqual(record.TXT, []string{"version=1", "instance=GNGZHCRUORSISNQHIEUT6H7KZQ", "pairing=1"}) ||
			!reflect.DeepEqual(ipStrings(record.Addresses), []string{"192.168.1.68"}) {
			t.Fatalf("record = %#v", record)
		}
	case <-ctx.Done():
		t.Fatal("Browse() did not emit the resolved Windows record")
	}
}

func TestDNSHelperProcess(t *testing.T) {
	if os.Getenv("REMOTE_DOCKER_DNS_HELPER") != "1" {
		return
	}
	separator := 0
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index + 1
			break
		}
	}
	args := os.Args[separator:]
	if len(args) == 0 {
		os.Exit(2)
	}
	switch args[0] {
	case "-B":
		fmt.Println("21:14:22.262  Add        2  11 local. _remote-docker._tcp. remote-docker-GNGZHCRUORSISNQHIEUT6H7KZQ")
	case "-L":
		fmt.Println("21:15:39.351  remote-docker-GNGZHCRUORSISNQHIEUT6H7KZQ._remote-docker._tcp.local. can be reached at DESKTOP-15A3GMV.local.:54397 (interface 11)")
		fmt.Println(" version=1 instance=GNGZHCRUORSISNQHIEUT6H7KZQ pairing=1")
	case "-G":
		fmt.Println("21:16:00.544  Add  40000003  11  DESKTOP-15A3GMV.local.  2A02:2168:B602:1300:2075:F75D:86B6:1365%<0>  120")
		fmt.Println("21:16:00.544  Add  40000003  11  DESKTOP-15A3GMV.local.  192.168.1.68  120")
	default:
		os.Exit(2)
	}
	time.Sleep(30 * time.Second)
}

func helperDNSCommand(ctx context.Context, _ string, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestDNSHelperProcess", "--"}, args...)...)
	command.Env = append(os.Environ(), "REMOTE_DOCKER_DNS_HELPER=1")
	return command
}

func TestSystemLocalAddressPolicy(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "fd00::1"} {
		if !isSystemLocalAddress(net.ParseIP(value)) {
			t.Fatalf("isSystemLocalAddress(%s) = false", value)
		}
	}
	for _, value := range []string{"0.0.0.0", "8.8.8.8", "169.254.1.1", "fe80::1", "224.0.0.251"} {
		if isSystemLocalAddress(net.ParseIP(value)) {
			t.Fatalf("isSystemLocalAddress(%s) = true", value)
		}
	}
}
