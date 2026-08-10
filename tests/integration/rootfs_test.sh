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
    etc/systemd/system/docker.service.d/remote-docker.conf \
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
  require_contains "${source_root}/build-rootfs.sh" '--output .*type=tar,dest='
  require_not_contains "${source_root}/build-rootfs.sh" 'type=local'
  require_contains "${source_root}/Containerfile" 'COPY --from=syncthing-asset'
  require_contains "${source_root}/Containerfile" '/var/lib/remote-docker-private'
  require_contains "${source_root}/Containerfile" 'systemd-sysv'
  require_contains "${source_root}/Containerfile" '^[[:space:]]+dbus[[:space:]]*\\$'
  require_contains "${source_root}/Containerfile" '^[[:space:]]+dbus-user-session[[:space:]]*\\$'
  require_contains "${source_root}/Containerfile" '^[[:space:]]+kmod[[:space:]]*\\$'
  require_contains "${source_root}/Containerfile" '^[[:space:]]+tzdata[[:space:]]*\\$'
  require_not_contains "${source_root}/Containerfile" 'github\.com/syncthing|curl .*syncthing'
  require_not_contains "${source_root}/etc/docker/daemon.json" '2375|2376|tcp://'
  require_not_contains "${source_root}/etc/docker/daemon.json" '"hosts"[[:space:]]*:'
  require_contains "${source_root}/etc/systemd/system/docker.service.d/remote-docker.conf" '^ExecStart=/usr/bin/dockerd --host=unix:///var/run/docker\.sock --containerd=/run/containerd/containerd\.sock$'
  require_contains "${source_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^PasswordAuthentication no$'
  require_contains "${source_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^HostKey /run/remote-docker/ssh_host_ed25519_key$'
  require_not_contains "${source_root}/etc/systemd/system/ssh.service.d/remote-docker.conf" 'ssh-keygen|/etc/remote-docker/ssh_host_ed25519_key'
  require_contains "${source_root}/etc/systemd/system/syncthing@.service" '--config=/run/remote-docker/syncthing'
  require_contains "${source_root}/etc/systemd/system/syncthing@.service" '--data=/var/lib/remote-docker/syncthing/data'
  require_not_contains "${source_root}/etc/systemd/system/syncthing@.service" '--no-default-folder'
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
[[ -d "${extract_root}/var/lib/remote-docker-private" ]] || fail "missing root-owned encrypted identity directory"
require_contains "${extract_root}/etc/passwd" '^remote-docker:[^:]*:[0-9]+:[0-9]+:[^:]*:[^:]*:/bin/sh$'
require_contains "${extract_root}/etc/group" '^docker:'
require_contains "${extract_root}/etc/shadow" '^remote-docker:!'
require_contains "${extract_root}/etc/wsl.conf" '^systemd=true$'
require_contains "${extract_root}/etc/wsl.conf" '^default=remote-docker$'
require_contains "${extract_root}/var/lib/dpkg/status" '^Package: systemd-sysv$'
require_contains "${extract_root}/var/lib/dpkg/status" '^Package: dbus$'
require_contains "${extract_root}/var/lib/dpkg/status" '^Package: dbus-user-session$'
require_contains "${extract_root}/var/lib/dpkg/status" '^Package: kmod$'
require_contains "${extract_root}/var/lib/dpkg/status" '^Package: tzdata$'
require_file "${extract_root}/usr/share/zoneinfo/zone.tab"
require_not_contains "${extract_root}/etc/pam.d/login" 'pam_lastlog\.so'
[[ -L "${extract_root}/sbin/init" ]] || fail "missing systemd /sbin/init link"
[[ "$(readlink "${extract_root}/sbin/init")" == '../lib/systemd/systemd' ]] || fail "unexpected /sbin/init target"

for path in \
  usr/bin/docker \
  usr/bin/dbus-daemon \
  usr/bin/kmod \
  usr/local/bin/syncthing \
  usr/local/bin/remote-docker-remote \
  usr/local/libexec/remote-docker/authorized-command \
  etc/systemd/system/docker.service.d/remote-docker.conf \
  etc/systemd/system/remote-docker.target \
  etc/systemd/system/remote-docker-remote.service \
  etc/systemd/system/syncthing@.service \
  etc/systemd/system/syncthing@remote-docker.service.d/override.conf \
  etc/ssh/sshd_config.d/remote-docker.conf \
  etc/docker/daemon.json; do
  require_file "${extract_root}/${path}"
done

if [[ "$(id -u)" -eq 0 ]]; then
  for root_owned_path in \
    . \
    dev \
    etc \
    run \
    usr \
    var \
    var/lib \
    var/log \
    var/lib/remote-docker-private; do
    [[ "$(stat -c '%u:%g' "${extract_root}/${root_owned_path}")" == '0:0' ]] ||
      fail "${root_owned_path} is not owned by root:root"
  done
  remote_uid="$(awk -F: '$1 == "remote-docker" { print $3 }' "${extract_root}/etc/passwd")"
  remote_gid="$(awk -F: '$1 == "remote-docker" { print $4 }' "${extract_root}/etc/passwd")"
  [[ -n "${remote_uid}" && -n "${remote_gid}" ]] || fail "remote-docker numeric ownership is unavailable"
  for service_owned_path in \
    Users \
    home/remote-docker \
    var/lib/remote-docker; do
    [[ "$(stat -c '%u:%g' "${extract_root}/${service_owned_path}")" == "${remote_uid}:${remote_gid}" ]] ||
      fail "${service_owned_path} is not owned by remote-docker"
  done
fi

for private_path in \
  etc/remote-docker/ssh_host_ed25519_key \
  var/lib/remote-docker/syncthing/cert.pem \
  var/lib/remote-docker/syncthing/key.pem; do
  [[ ! -e "${extract_root}/${private_path}" ]] || fail "persistent private identity exists at ${private_path}"
done

require_not_contains "${extract_root}/etc/docker/daemon.json" '2375|2376|tcp://'
require_not_contains "${extract_root}/etc/docker/daemon.json" '"hosts"[[:space:]]*:'
require_contains "${extract_root}/etc/systemd/system/docker.service.d/remote-docker.conf" '^ExecStart=/usr/bin/dockerd --host=unix:///var/run/docker\.sock --containerd=/run/containerd/containerd\.sock$'
require_not_contains "${extract_root}/etc/systemd/system/syncthing@.service" '--no-default-folder'
require_contains "${extract_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^PermitTTY no$'
require_contains "${extract_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^X11Forwarding no$'
require_contains "${extract_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^AllowAgentForwarding no$'
require_contains "${extract_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^PermitOpen 127\.0\.0\.1:\*$'
require_contains "${extract_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^PermitListen 127\.0\.0\.1:\*$'
require_contains "${extract_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^PasswordAuthentication no$'
require_contains "${extract_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^PermitRootLogin no$'
require_contains "${extract_root}/etc/ssh/sshd_config.d/remote-docker.conf" '^HostKey /run/remote-docker/ssh_host_ed25519_key$'
require_not_contains "${extract_root}/etc/systemd/system/ssh.service.d/remote-docker.conf" 'ssh-keygen|/etc/remote-docker/ssh_host_ed25519_key'
require_not_contains "${extract_root}/usr/local/libexec/remote-docker/authorized-command" 'eval|bash -c|sh -c'

if [[ "$(uname -s)" == "Linux" ]]; then
  chroot "${extract_root}" /usr/local/bin/syncthing --version | grep -F 'v2.1.1' >/dev/null
  syncthing_help="$(chroot "${extract_root}" /usr/local/bin/syncthing serve --help 2>&1)"
  for flag in --no-browser --no-restart --no-upgrade --config --data --gui-address; do
    grep -F -- "${flag}" <<<"${syncthing_help}" >/dev/null || fail "Syncthing 2.1.1 does not support ${flag}"
  done
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
