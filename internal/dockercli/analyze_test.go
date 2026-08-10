package dockercli

import (
	"context"
	"reflect"
	"testing"
)

func TestAnalyzeRunPortsAndUnsupportedNetworking(t *testing.T) {
	analysis, err := Analyze(context.Background(), Invocation{Args: []string{
		"--context", "remote-docker", "run",
		"-p", "8080:80",
		"--publish=127.0.0.1:9090:90/tcp",
		"-p", "80",
		"-p", "0:81",
		"-p", "5353:53/udp",
		"--network=host",
		"alpine",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantPorts := []Port{
		{HostPort: 8080, ContainerPort: 80},
		{HostIP: "127.0.0.1", HostPort: 9090, ContainerPort: 90},
	}
	if !reflect.DeepEqual(analysis.StaticTCPPorts, wantPorts) {
		t.Fatalf("StaticTCPPorts = %#v, want %#v", analysis.StaticTCPPorts, wantPorts)
	}
	wantReasons := []ReasonCode{ReasonUnsupportedUDP, ReasonHostNetworking}
	if got := reasonCodes(analysis.Unsupported); !reflect.DeepEqual(got, wantReasons) {
		t.Fatalf("Unsupported = %#v, want %#v", got, wantReasons)
	}
}

func TestAnalyzeDoesNotTreatBuildContextAsBindMount(t *testing.T) {
	analysis, err := Analyze(context.Background(), Invocation{Args: []string{"build", "-f", "Dockerfile", "."}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(analysis.BindSources) != 0 {
		t.Fatalf("build context became bind source: %#v", analysis.BindSources)
	}
	if !analysis.NeedsEngine {
		t.Fatal("docker build did not require the engine")
	}
}

func reasonCodes(reasons []Reason) []ReasonCode {
	if len(reasons) == 0 {
		return nil
	}
	result := make([]ReasonCode, 0, len(reasons))
	for _, reason := range reasons {
		result = append(result, reason.Code)
	}
	return result
}
