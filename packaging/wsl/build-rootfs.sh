#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
dist_dir="${repo_root}/dist"
artifact="${dist_dir}/remote-docker-rootfs.tar.zst"
build_dir="$(mktemp -d)"
artifact_tmp="${build_dir}/remote-docker-rootfs.tar.zst"
rootfs_tar="${build_dir}/remote-docker-rootfs.tar"
syncthing_version="2.1.1"
syncthing_sha256="0b960a67a0391156c2ca45943ed1ceaad9ae1fc3772d967e6aafc5a7c662565d"
syncthing_archive="syncthing-linux-amd64-v${syncthing_version}.tar.gz"

cleanup() {
  rm -rf "${build_dir}"
}
trap cleanup EXIT

command -v docker >/dev/null 2>&1 || { echo "docker CLI is required" >&2; exit 1; }
command -v zstd >/dev/null 2>&1 || { echo "zstd is required" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 1; }
docker info >/dev/null

asset_dir="${build_dir}/assets"
asset_path="${asset_dir}/${syncthing_archive}"
cache_root="${REMOTE_DOCKER_ASSET_CACHE:-}"
mkdir -p "${asset_dir}"
if [[ -n "${cache_root}" ]]; then
  [[ "${cache_root}" == /* ]] || { echo "asset cache path must be absolute" >&2; exit 1; }
  [[ ! -L "${cache_root}/${syncthing_archive}" ]] || { echo "asset cache entry must not be a symlink" >&2; exit 1; }
fi
if [[ -n "${cache_root}" && -f "${cache_root}/${syncthing_archive}" ]]; then
  cp "${cache_root}/${syncthing_archive}" "${asset_path}"
else
  curl --proto '=https' --tlsv1.2 --fail --location --silent --show-error \
    --connect-timeout 20 --max-time 600 --retry 3 \
    "https://github.com/syncthing/syncthing/releases/download/v${syncthing_version}/${syncthing_archive}" \
    --output "${asset_path}"
fi
if command -v sha256sum >/dev/null 2>&1; then
  downloaded_hash="$(sha256sum "${asset_path}" | awk '{print $1}')"
else
  downloaded_hash="$(shasum -a 256 "${asset_path}" | awk '{print $1}')"
fi
[[ "${downloaded_hash}" == "${syncthing_sha256}" ]] || { echo "Syncthing SHA-256 mismatch" >&2; exit 1; }
if [[ -n "${cache_root}" && ! -e "${cache_root}/${syncthing_archive}" ]]; then
  mkdir -p "${cache_root}"
  cp "${asset_path}" "${cache_root}/${syncthing_archive}"
fi

# Public base images do not need the user's Docker credentials. A temporary
# config also prevents a locked desktop credential helper from blocking CI or
# unattended builds, while the explicit host preserves the selected daemon.
docker_host="$(docker context inspect --format '{{.Endpoints.docker.Host}}')"
runtime_docker_config="${build_dir}/docker-config"
mkdir -p "${runtime_docker_config}"
printf '%s\n' '{"auths":{}}' > "${runtime_docker_config}/config.json"
default_docker_config="${DOCKER_CONFIG:-${HOME}/.docker}"
if [[ -d "${default_docker_config}/cli-plugins" ]]; then
  ln -s "${default_docker_config}/cli-plugins" "${runtime_docker_config}/cli-plugins"
fi

mkdir -p "${dist_dir}"
DOCKER_CONFIG="${runtime_docker_config}" DOCKER_HOST="${docker_host}" docker buildx build \
  --builder default \
  --file "${repo_root}/packaging/wsl/Containerfile" \
  --platform linux/amd64 \
  --build-context "syncthing-asset=${asset_dir}" \
  --output "type=tar,dest=${rootfs_tar}" \
  "${repo_root}"

zstd -q -T0 -19 "${rootfs_tar}" -o "${artifact_tmp}"
mv "${artifact_tmp}" "${artifact}"

if command -v sha256sum >/dev/null 2>&1; then
  artifact_hash="$(sha256sum "${artifact}" | awk '{print $1}')"
else
  artifact_hash="$(shasum -a 256 "${artifact}" | awk '{print $1}')"
fi
printf '%s  %s\n' "${artifact_hash}" "$(basename "${artifact}")" > "${artifact}.sha256"

echo "built ${artifact}"
