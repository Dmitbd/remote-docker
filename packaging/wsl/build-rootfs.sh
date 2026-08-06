#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
dist_dir="${repo_root}/dist"
artifact="${dist_dir}/remote-docker-rootfs.tar.zst"
build_dir="$(mktemp -d)"

cleanup() {
  rm -rf "${build_dir}"
}
trap cleanup EXIT

command -v docker >/dev/null 2>&1 || { echo "docker CLI is required" >&2; exit 1; }
command -v zstd >/dev/null 2>&1 || { echo "zstd is required" >&2; exit 1; }
docker info >/dev/null

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
  --output "type=local,dest=${build_dir}/rootfs" \
  "${repo_root}"

tar -C "${build_dir}/rootfs" -cf - . | zstd -q -T0 -19 -o "${artifact}"

if command -v sha256sum >/dev/null 2>&1; then
  artifact_hash="$(sha256sum "${artifact}" | awk '{print $1}')"
else
  artifact_hash="$(shasum -a 256 "${artifact}" | awk '{print $1}')"
fi
printf '%s  %s\n' "${artifact_hash}" "$(basename "${artifact}")" > "${artifact}.sha256"

echo "built ${artifact}"
