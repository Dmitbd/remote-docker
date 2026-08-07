package diagnostics

import (
	"errors"
	"strings"
	"testing"
)

func TestPublicReasonUsesGenericFallbackForUntrustedErrors(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		input  string
	}{
		{name: "generic env assignment", secret: "quoted value with spaces", input: `ORDINARY_SETTING="quoted value with spaces"`},
		{name: "single quoted env assignment", secret: "single quoted secret", input: `HOME_PATH='single quoted secret'`},
		{name: "escaped registry json", secret: "registry-secret", input: `registry said {\"auth\":\"registry-secret\",\"email\":\"dev@example.test\"}`},
		{name: "folded bearer header", secret: "folded-bearer-secret", input: "Authorization: Bearer\r\n folded-bearer-secret"},
		{name: "crlf bearer value", secret: "crlf-bearer-secret", input: "Authorization:\r\n\tBearer crlf-bearer-secret"},
		{name: "delimiter heavy URL password", secret: "p@ss:word;with,delimiters", input: `https://operator:p@ss:word;with,delimiters@windows-peer:8384/rest`},
		{name: "PKCS8 PEM", secret: "pkcs8-private-material", input: "-----BEGIN PRIVATE KEY-----\npkcs8-private-material\n-----END PRIVATE KEY-----"},
		{name: "RSA PEM", secret: "rsa-private-material", input: "-----BEGIN RSA PRIVATE KEY-----\nrsa-private-material\n-----END RSA PRIVATE KEY-----"},
		{name: "EC PEM", secret: "ec-private-material", input: "-----BEGIN EC PRIVATE KEY-----\nec-private-material\n-----END EC PRIVATE KEY-----"},
		{name: "OPENSSH PEM", secret: "openssh-private-material", input: "-----BEGIN OPENSSH PRIVATE KEY-----\nopenssh-private-material\n-----END OPENSSH PRIVATE KEY-----"},
		{name: "DSA PEM", secret: "dsa-private-material", input: "-----BEGIN DSA PRIVATE KEY-----\ndsa-private-material\n-----END DSA PRIVATE KEY-----"},
		{name: "encrypted PEM", secret: "encrypted-private-material", input: "-----BEGIN ENCRYPTED PRIVATE KEY-----\nencrypted-private-material\n-----END ENCRYPTED PRIVATE KEY-----"},
		{name: "Syncthing API key", secret: "sync-api-key-396", input: "X-API-Key: sync-api-key-396"},
		{name: "pairing token", secret: "pairing-token-417", input: "pairing_token=pairing-token-417"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReasonForError(errors.New(tt.input), ReasonCheckFailed)
			if got != string(ReasonCheckFailed) {
				t.Fatalf("ReasonForError() = %q, want stable generic reason", got)
			}
			if strings.Contains(got, tt.secret) {
				t.Fatalf("public reason retained secret %q: %q", tt.secret, got)
			}
		})
	}
}

func TestPublicReasonPreservesOnlyExplicitlyAllowlistedReason(t *testing.T) {
	if got := ReasonForError(NewPublicError(ReasonDockerSocketNotReady), ReasonCheckFailed); got != string(ReasonDockerSocketNotReady) {
		t.Fatalf("ReasonForError() = %q, want explicitly safe reason", got)
	}
	if got := ReasonForError(NewPublicError(Reason("not allowlisted: token=secret")), ReasonCheckFailed); got != string(ReasonCheckFailed) {
		t.Fatalf("ReasonForError() = %q, want generic fallback for unknown typed value", got)
	}
}
