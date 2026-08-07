package diagnostics

import (
	"errors"
	"regexp"
	"strings"
)

var (
	pemPrivateKeyPattern = regexp.MustCompile(`(?s)-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----.*?-----END (?:[A-Z0-9 ]+ )?PRIVATE KEY-----`)
	envSecretPattern     = regexp.MustCompile(`(?mi)^([A-Z_][A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASS|API_KEY|APIKEY|AUTH|CREDENTIAL)[A-Z0-9_]*)\s*=\s*[^\r\n]*$`)
	bearerPattern        = regexp.MustCompile(`(?mi)(authorization\s*:\s*bearer\s+)[^\s\r\n,;]+`)
	headerSecretPattern  = regexp.MustCompile(`(?mi)((?:x-api-key|x-api-token|syncthing-api-key|pairing-token)\s*:\s*)[^\r\n]+`)
	jsonSecretPattern    = regexp.MustCompile(`(?i)("(?:auth|identitytoken|registrytoken|password|token|api_key|apikey|pairing_token)"\s*:\s*")[^"]*(")`)
	urlCredentialPattern = regexp.MustCompile(`([a-z][a-z0-9+.-]*://)[^/@\s:]+:[^/@\s]+@`)
	labelSecretPattern   = regexp.MustCompile(`(?mi)((?:pairing[_ -]?token|syncthing[_ -]?(?:api[_ -]?)?key|(?:api[_ -]?)?key|token|secret|password|credential)\s*[:=]\s*)[^\s\r\n,;]+`)
)

// RedactReason returns a stable diagnostic reason with common credentials
// removed, including multiline private keys and composite transport errors.
func RedactReason(err error) string {
	if err == nil {
		return ""
	}
	return RedactString(err.Error())
}

// RedactString removes credential-bearing values while retaining useful,
// non-sensitive context such as host names and the operation that failed.
func RedactString(reason string) string {
	if reason == "" {
		return ""
	}
	redacted := pemPrivateKeyPattern.ReplaceAllString(reason, "[REDACTED PRIVATE KEY]")
	redacted = envSecretPattern.ReplaceAllString(redacted, "${1}=[REDACTED]")
	redacted = bearerPattern.ReplaceAllString(redacted, "${1}[REDACTED]")
	redacted = headerSecretPattern.ReplaceAllString(redacted, "${1}[REDACTED]")
	redacted = jsonSecretPattern.ReplaceAllString(redacted, "${1}[REDACTED]${2}")
	redacted = urlCredentialPattern.ReplaceAllString(redacted, "${1}[REDACTED]@")
	redacted = labelSecretPattern.ReplaceAllString(redacted, "${1}[REDACTED]")
	return strings.TrimSpace(redacted)
}

// PublicError removes a wrapped diagnostic cause before it crosses a public
// boundary. Callers may use errors.Is on the returned stable sentinel.
func PublicError(public error) error {
	if public == nil {
		return nil
	}
	return errors.New(public.Error())
}
