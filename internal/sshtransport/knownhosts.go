package sshtransport

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
)

var ErrHostKeyChanged = errors.New("paired SSH host key changed")

// PinKnownHost records a pairing-confirmed host key and never auto-replaces it.
func PinKnownHost(path, alias, authorizedKey string) error {
	if _, err := hostAlias(strings.TrimPrefix(alias, "remote-docker-device-")); err != nil ||
		!strings.HasPrefix(alias, "remote-docker-device-") {
		return errors.New("invalid known-host alias")
	}
	publicKey, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil || len(strings.TrimSpace(string(rest))) != 0 || publicKey.Type() != ssh.KeyAlgoED25519 {
		return errors.New("invalid Ed25519 SSH host public key")
	}
	canonicalKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey)))
	line := alias + " " + canonicalKey

	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read managed known_hosts: %w", err)
	}
	for _, existingLine := range strings.Split(string(existing), "\n") {
		fields := strings.Fields(existingLine)
		if len(fields) == 0 || !hostListContains(fields[0], alias) {
			continue
		}
		if len(fields) >= 3 && fields[1]+" "+fields[2] == canonicalKey {
			return nil
		}
		return fmt.Errorf("%w; remove pairing and pair the device again", ErrHostKeyChanged)
	}

	content := string(existing)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += line + "\n"
	return writePrivateFile(path, []byte(content))
}

func hostListContains(hostList, alias string) bool {
	for _, host := range strings.Split(hostList, ",") {
		if host == alias {
			return true
		}
	}
	return false
}
