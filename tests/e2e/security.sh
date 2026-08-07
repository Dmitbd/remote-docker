#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/e2e/lib.sh
source "${script_dir}/lib.sh"

e2e_require_opt_in
e2e_assert_remote_engine
e2e_require_command ssh

windows_host="${REMOTE_DOCKER_E2E_WINDOWS_HOST:-}"
mac_host="${REMOTE_DOCKER_E2E_MAC_HOST:-}"
third_host="${REMOTE_DOCKER_E2E_THIRD_HOST:-}"
[[ -n "${windows_host}" ]] || e2e_fail 'REMOTE_DOCKER_E2E_WINDOWS_HOST must be the paired Windows private IP'
[[ -n "${mac_host}" ]] || e2e_fail 'REMOTE_DOCKER_E2E_MAC_HOST must be the paired Mac private IP'
[[ -n "${third_host}" ]] || e2e_fail 'REMOTE_DOCKER_E2E_THIRD_HOST must be an SSH-accessible third device on the same LAN'

for port in 2375 2376; do
  if ssh -o BatchMode=yes "${third_host}" nc -z -w 3 "${windows_host}" "${port}"; then
    e2e_fail "Docker API port ${port} is reachable from a third LAN device"
  fi
done

for host in "${windows_host}" "${mac_host}"; do
  if ssh -o BatchMode=yes "${third_host}" nc -z -w 3 "${host}" 8384; then
    e2e_fail "Syncthing REST port 8384 is reachable on ${host} from a third LAN device"
  fi
done

if ssh -o BatchMode=yes "${third_host}" \
  ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -p 49222 "remote-docker@${windows_host}" true; then
  e2e_fail 'an unknown SSH key authenticated to the Windows bridge'
fi

config_root="${HOME}/Library/Application Support/RemoteDocker"
if find "${config_root}" -type f \( -name cert.pem -o -name key.pem \) -print -quit | grep -q .; then
  e2e_fail 'plaintext Syncthing identity persisted below the application data directory'
fi

marker="remote-docker-e2e-secret-$$"
doctor_output="$(REMOTE_DOCKER_E2E_SECRET_MARKER="${marker}" remote-docker doctor --json)"
[[ "${doctor_output}" != *"${marker}"* ]] || e2e_fail 'diagnostics exposed an environment secret marker'

socket_path="${config_root}/agent.sock"
if [[ -S "${socket_path}" ]]; then
  mode="$(stat -f '%Lp' "${socket_path}")"
  [[ "${mode}" == '600' ]] || e2e_fail "local control socket mode is ${mode}, expected 600"
fi

[[ "${REMOTE_DOCKER_E2E_REVOKE_CONFIRMED:-}" == 'I_UNDERSTAND_THIS_REMOVES_THE_PAIRING' ]] ||
  e2e_fail 'set REMOTE_DOCKER_E2E_REVOKE_CONFIRMED=I_UNDERSTAND_THIS_REMOVES_THE_PAIRING to verify revocation last'
remote-docker unpair >/dev/null || e2e_fail 'managed pairing revocation failed'
if docker info >/dev/null 2>&1; then
  e2e_fail 'Docker remained reachable after managed pairing revocation'
fi

e2e_note 'LAN exposure, unknown-key rejection, local secret checks, and pairing revocation: PASS'
e2e_note 'The acceptance pairing was removed intentionally; pair the computers again before further use.'
