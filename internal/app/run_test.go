package app

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var stdout bytes.Buffer

	code := Run([]string{"version"}, &stdout, io.Discard)

	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != "remote-docker dev" {
		t.Fatalf("stdout = %q, want %q", got, "remote-docker dev")
	}
}

func TestRunUnknownCommandPrintsUsage(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run([]string{"unknown"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "unknown command: unknown") {
		t.Fatalf("stderr = %q, want unknown command message", got)
	}
	if got := stderr.String(); !strings.Contains(got, "usage: remote-docker <command>") {
		t.Fatalf("stderr = %q, want usage", got)
	}
}
