#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

required_scripts=(
  tests/e2e/lib.sh
  tests/e2e/docker_compatibility.sh
  tests/e2e/workspace_sync.sh
  tests/e2e/reconnect.sh
  tests/e2e/security.sh
)

for relative in "${required_scripts[@]}"; do
  path="${repo_root}/${relative}"
  [[ -f "${path}" ]] || { printf 'missing acceptance script: %s\n' "${relative}" >&2; exit 1; }
  bash -n "${path}"
done

for relative in "${required_scripts[@]:1}"; do
  path="${repo_root}/${relative}"
  grep -q 'e2e_require_opt_in' "${path}" || { printf 'missing destructive-test opt-in: %s\n' "${relative}" >&2; exit 1; }
  grep -q 'e2e_assert_remote_engine' "${path}" || { printf 'missing remote-engine identity gate: %s\n' "${relative}" >&2; exit 1; }
done

for relative in README.md docs/INSTALL.md docs/TROUBLESHOOTING.md; do
  [[ -f "${repo_root}/${relative}" ]] || { printf 'missing public documentation: %s\n' "${relative}" >&2; exit 1; }
done

grep -q 'Managed by Remote Docker' "${repo_root}/tests/e2e/lib.sh"
grep -q 'ssh://remote-docker-device-' "${repo_root}/tests/e2e/lib.sh"
grep -q 'docker compose' "${repo_root}/tests/e2e/docker_compatibility.sh"
grep -q 'container-to-mac' "${repo_root}/tests/e2e/workspace_sync.sh"
grep -q 'REMOTE_DOCKER_E2E_REBOOT_CONFIRMED' "${repo_root}/tests/e2e/reconnect.sh"
grep -q 'REMOTE_DOCKER_E2E_RELAY_PORT' "${repo_root}/tests/e2e/reconnect.sh"
grep -q 'REMOTE_DOCKER_E2E_THIRD_HOST' "${repo_root}/tests/e2e/security.sh"
grep -q 'REMOTE_DOCKER_E2E_MAC_HOST' "${repo_root}/tests/e2e/security.sh"
grep -q 'REMOTE_DOCKER_E2E_REVOKE_CONFIRMED' "${repo_root}/tests/e2e/security.sh"

private_pattern='co''work|rata''toskr|s''ber|mid''gard|ygg''drasil'
if rg -n -i "${private_pattern}" \
  "${repo_root}/README.md" "${repo_root}/docs" "${repo_root}/tests/e2e"; then
  printf 'public acceptance content contains a private project reference\n' >&2
  exit 1
fi

printf 'public acceptance contract: PASS\n'
