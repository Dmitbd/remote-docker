#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
artifact="${REMOTE_DOCKER_ROOTFS:-${repo_root}/dist/remote-docker-rootfs.tar.zst}"

fail() {
  echo "rootfs contract failed: $*" >&2
  exit 1
}

require_file() {
  [[ -f "${1}" ]] || fail "missing ${1}"
}

require_contains() {
  local path="$1"
  local pattern="$2"
  grep -Eq -- "${pattern}" "${path}" || fail "${path} does not match ${pattern}"
}

require_not_contains() {
  local path="$1"
  local pattern="$2"
  if grep -Eq -- "${pattern}" "${path}"; then
    fail "${path} unexpectedly matches ${pattern}"
  fi
}

if [[ "${1:-}" == "--source" ]]; then
  source_root="${repo_root}/packaging/wsl"
  for relative in \
    Containerfile \
    build-rootfs.sh \
    etc/wsl.conf \
    etc/docker/daemon.json \
    etc/ssh/sshd_config.d/remote-docker.conf \
    etc/systemd/system/remote-docker.target \
    etc/systemd/system/remote-docker-remote.service \
    etc/systemd/system/syncthing@.service \
    etc/systemd/system/syncthing@remote-docker.service.d/override.conf \
    usr/local/libexec/remote-docker/authorized-command; do
    require_file "${source_root}/${relative}"
  done
  require_contains "${source_root}/Containerfile" 'SYNCTHING_VERSION=2\.1\.1'
  require_contains "${source_root}/Containerfile" '0b960a67a0391156c2ca45943ed1ceaad9ae1fc3772d967e6aafc5a7c662565d'
  require_contains "${source_root}/build-rootfs.sh" 'REMOTE_DOCKER_ASSET_CACHE'
  require_contains "${source_root}/build-rootfs.sh" '--build-context .*syncthing-asset='
  require_contains "${source_root}/build-rootfs.sh" 'artifact_tmp='
  require_contains "${source_root}/build-rootfs.sh" 'mv .*artifact_tmp.*artifact'
  require_contains "${source_root}/Containerfile" 'COPY --from=syncthing-asset'
  require_not_contains "${source_root}/Containerfile" 'github\.com/syncthing|curl .*syncthing'
  require_not_contains "${source_root}/etc/docker/daemon.json" '2375|2376|tcp://'
  require_contains "${source_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^PasswordAuthentication no$'
  require_contains "${source_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^PermitOpen 127\.0\.0\.1:\*$'
  require_contains "${source_root}/usr/local/libexec/remote-docker/authorized-command" 'docker system dial-stdio'
  require_contains "${source_root}/usr/local/libexec/remote-docker/authorized-command" 'remote-docker-remote rpc'
  echo "rootfs source contract: PASS"
  exit 0
fi

require_file "${artifact}"
command -v zstd >/dev/null 2>&1 || fail "zstd is required"

extract_root="$(mktemp -d)"
cleanup() {
  rm -rf "${extract_root}"
}
trap cleanup EXIT

zstd -q -dc "${artifact}" | tar -xf - -C "${extract_root}"

require_file "${extract_root}/etc/passwd"
require_file "${extract_root}/etc/shadow"
require_contains "${extract_root}/etc/passwd" '^remote-docker:[^:]*:[0-9]+:[0-9]+:[^:]*:[^:]*:/bin/sh$'
require_contains "${extract_root}/etc/group" '^docker:'
require_contains "${extract_root}/etc/shadow" '^remote-docker:!'
require_contains "${extract_root}/etc/wsl.conf" '^systemd=true$'
require_contains "${extract_root}/etc/wsl.conf" '^default=remote-docker$'

for path in \
  usr/bin/docker \
  usr/local/bin/syncthing \
  usr/local/bin/remote-docker-remote \
  usr/local/libexec/remote-docker/authorized-command \
  etc/systemd/system/remote-docker.target \
  etc/systemd/system/remote-docker-remote.service \
  etc/systemd/system/syncthing@.service \
  etc/systemd/system/syncthing@remote-docker.service.d/override.conf \
  etc/ssh/sshd_config.d/remote-docker.conf \
  etc/docker/daemon.json; do
  require_file "${extract_root}/${path}"
done

require_not_contains "${extract_root}/etc/docker/daemon.json" '2375|2376|tcp://'
require_contains "${extract_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^PermitTTY no$'
require_contains "${extract_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^X11Forwarding no$'
require_contains "${extract_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^AllowAgentForwarding no$'
require_contains "${extract_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^PermitOpen 127\.0\.0\.1:\*$'
require_contains "${extract_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^PermitListen 127\.0\.0\.1:\*$'
require_contains "${extract_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^PasswordAuthentication no$'
require_contains "${extract_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^PermitRootLogin no$'
require_not_contains "${extract_root}/usr/local/libexec/remote-docker/authorized-command" 'eval|bash -c|sh -c'

if [[ "$(uname -s)" == "Linux" ]]; then
  chroot "${extract_root}" /usr/local/bin/syncthing --version | grep -F 'v2.1.1' >/dev/null
  chroot "${extract_root}" /usr/local/bin/remote-docker-remote health | grep -F '"status":"ok"' >/dev/null
fi

manifest="${artifact}.sha256"
require_file "${manifest}"
expected_hash="$(awk '{print $1}' "${manifest}")"
if command -v sha256sum >/dev/null 2>&1; then
  actual_hash="$(sha256sum "${artifact}" | awk '{print $1}')"
else
  actual_hash="$(shasum -a 256 "${artifact}" | awk '{print $1}')"
fi
[[ "${actual_hash}" == "${expected_hash}" ]] || fail "artifact SHA-256 does not match manifest"

echo "rootfs artifact contract: PASS"
