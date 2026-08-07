#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/e2e/lib.sh
source "${script_dir}/lib.sh"

e2e_require_opt_in
e2e_assert_remote_engine
workspace="$(e2e_require_workspace)"
image="${REMOTE_DOCKER_E2E_IMAGE:-alpine:3.21}"
case_dir="$(mktemp -d "${workspace}/.remote-docker-e2e-sync.XXXXXX")"
e2e_assert_owned_temp "${workspace}" "${case_dir}"

cleanup() {
  e2e_assert_owned_temp "${workspace}" "${case_dir}"
  rm -rf "${case_dir}"
}
trap cleanup EXIT INT TERM

printf 'initial-from-mac\n' >"${case_dir}/mac-to-windows.txt"
docker run --rm --mount "type=bind,src=${case_dir},dst=/workspace" "${image}" \
  sh -c 'test "$(cat /workspace/mac-to-windows.txt)" = initial-from-mac'

printf 'incremental-from-mac\n' >"${case_dir}/mac-to-windows.txt"
docker run --rm --mount "type=bind,src=${case_dir},dst=/workspace" "${image}" \
  sh -c 'test "$(cat /workspace/mac-to-windows.txt)" = incremental-from-mac'

docker run --rm --mount "type=bind,src=${case_dir},dst=/workspace" "${image}" \
  sh -c 'printf "container-to-mac\n" >/workspace/container-to-mac.txt'
e2e_wait_until "${REMOTE_DOCKER_E2E_SYNC_TIMEOUT:-120}" 'container-generated file on Mac' \
  grep -q '^container-to-mac$' "${case_dir}/container-to-mac.txt"

mkdir -p "${case_dir}/node_modules"
printf 'must-not-sync\n' >"${case_dir}/node_modules/ignored.txt"
if docker run --rm --mount "type=bind,src=${case_dir},dst=/workspace" "${image}" test -e /workspace/node_modules/ignored.txt; then
  e2e_fail 'managed ignore pattern node_modules was synchronized'
fi

outside="$(mktemp -d /tmp/remote-docker-e2e-outside.XXXXXX)"
trap 'rm -rf "${outside}"; cleanup' EXIT INT TERM
if docker run --rm --mount "type=bind,src=${outside},dst=/outside" "${image}" true >/dev/null 2>&1; then
  e2e_fail 'an unregistered bind path reached the remote Engine'
fi
rm -rf "${outside}"

e2e_note 'two-way workspace synchronization and path policy: PASS'
