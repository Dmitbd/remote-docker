package diagnostics

import (
	"errors"
	"strings"
	"testing"
)

func TestRedactReasonRemovesSecretCorpusFromCompositeErrors(t *testing.T) {
	const (
		envSecret      = "env-value-741"
		bearerSecret   = "bearer-value-852"
		registrySecret = "cmVnaXN0cnktYXV0aC05NjM="
		urlSecret      = "url-password-174"
		pemSecret      = "pem-material-285"
		syncSecret     = "sync-api-key-396"
		pairingSecret  = "pairing-token-417"
	)

	err := errors.New("docker socket unavailable:\n" +
		"REMOTE_DOCKER_TOKEN=" + envSecret + "\n" +
		"Authorization: Bearer " + bearerSecret + "\n" +
		`registry response: {"auth":"` + registrySecret + `","email":"dev@example.test"}` + "\n" +
		"sync endpoint: https://operator:" + urlSecret + "@windows-peer:8384/rest\n" +
		"-----BEGIN PRIVATE KEY-----\n" + pemSecret + "\n-----END PRIVATE KEY-----\n" +
		"X-API-Key: " + syncSecret + "\n" +
		"pairing_token: " + pairingSecret)

	got := RedactReason(err)
	for _, secret := range []string{envSecret, bearerSecret, registrySecret, urlSecret, pemSecret, syncSecret, pairingSecret} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted reason retained secret %q: %s", secret, got)
		}
	}
	for _, stableFragment := range []string{"docker socket unavailable", "REMOTE_DOCKER_TOKEN=[REDACTED]", "registry response", "windows-peer", "[REDACTED PRIVATE KEY]"} {
		if !strings.Contains(got, stableFragment) {
			t.Fatalf("redacted reason = %q, want stable fragment %q", got, stableFragment)
		}
	}
}

func TestRedactReasonPreservesSafeReason(t *testing.T) {
	const reason = "Docker socket is not reachable"
	if got := RedactReason(errors.New(reason)); got != reason {
		t.Fatalf("RedactReason() = %q, want %q", got, reason)
	}
}
