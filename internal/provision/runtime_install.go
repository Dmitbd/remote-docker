package provision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
)

const managedWSLRuntimeInstallScript = `set -eu
expected="$1"
release="$(cat /etc/remote-docker-release)"
[ "$release" = "remote-docker-managed-v1" ] || exit 41
target=/usr/local/bin/remote-docker-remote
[ -L "$target" ] && exit 42
if [ -e "$target" ] && [ ! -f "$target" ]; then
  exit 42
fi
if [ -f "$target" ] && printf '%s  %s\n' "$expected" "$target" | sha256sum -c - >/dev/null 2>&1; then
  exit 0
fi
tmp="${target}.new"
trap 'rm -f "$tmp"' 0 HUP INT TERM
umask 077
cat > "$tmp"
printf '%s  %s\n' "$expected" "$tmp" | sha256sum -c - >/dev/null
chmod 0755 "$tmp"
chown root:root "$tmp"
mv -f "$tmp" "$target"
trap - 0 HUP INT TERM
`

// WSLRuntimeInstaller installs the packaged Linux helper into the managed WSL
// distribution without recreating the distribution or its data.
type WSLRuntimeInstaller struct {
	Runner        RuntimeIdentityRunner
	WSLBinary     string
	Distro        string
	CandidatePath string
	ChecksumPath  string
}

func (i WSLRuntimeInstaller) Install(ctx context.Context) error {
	digest, candidate, err := i.openVerifiedCandidate()
	if err != nil {
		return err
	}
	defer candidate.Close()
	runner := i.Runner
	if runner == nil {
		runner = execRuntimeIdentityRunner{}
	}
	binary := i.WSLBinary
	if binary == "" {
		binary = "wsl.exe"
	}
	distro := i.Distro
	if distro == "" {
		distro = defaultManagedDistro
	}
	err = runner.Run(ctx, RuntimeIdentityCommand{
		Binary: binary,
		Args: []string{
			"--distribution", distro, "--user", "root", "--exec", "/bin/sh", "-c",
			managedWSLRuntimeInstallScript, "remote-docker-runtime", digest,
		},
		Stdin: candidate, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		return errors.New("install packaged managed WSL runtime")
	}
	return nil
}

func (i WSLRuntimeInstaller) openVerifiedCandidate() (string, *os.File, error) {
	manifestInfo, err := os.Lstat(i.ChecksumPath)
	if err != nil || !manifestInfo.Mode().IsRegular() {
		return "", nil, errors.New("packaged managed WSL runtime checksum is unavailable")
	}
	manifest, err := os.ReadFile(i.ChecksumPath)
	if err != nil {
		return "", nil, errors.New("read packaged managed WSL runtime checksum")
	}
	fields := strings.Fields(string(manifest))
	if len(fields) == 0 || len(fields[0]) != sha256.Size*2 {
		return "", nil, errors.New("packaged managed WSL runtime checksum is invalid")
	}
	expected, err := hex.DecodeString(fields[0])
	if err != nil || len(expected) != sha256.Size {
		return "", nil, errors.New("packaged managed WSL runtime checksum is invalid")
	}
	candidateInfo, err := os.Lstat(i.CandidatePath)
	if err != nil || !candidateInfo.Mode().IsRegular() {
		return "", nil, errors.New("packaged managed WSL runtime is unavailable")
	}
	candidate, err := os.Open(i.CandidatePath)
	if err != nil {
		return "", nil, errors.New("open packaged managed WSL runtime")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, candidate); err != nil {
		candidate.Close()
		return "", nil, errors.New("hash packaged managed WSL runtime")
	}
	if !bytes.Equal(hash.Sum(nil), expected) {
		candidate.Close()
		return "", nil, errors.New("packaged managed WSL runtime checksum mismatch")
	}
	if _, err := candidate.Seek(0, io.SeekStart); err != nil {
		candidate.Close()
		return "", nil, errors.New("rewind packaged managed WSL runtime")
	}
	return strings.ToLower(fields[0]), candidate, nil
}
