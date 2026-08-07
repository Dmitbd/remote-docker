#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/e2e/lib.sh
source "${script_dir}/lib.sh"

e2e_require_opt_in
e2e_assert_remote_engine
e2e_require_command remote-docker
e2e_require_command curl
run_id="$(e2e_new_id)"
image="${REMOTE_DOCKER_E2E_IMAGE:-alpine:3.21}"
container="${run_id}-restart"
volume="${run_id}-state"
relay_port="${REMOTE_DOCKER_E2E_RELAY_PORT:-49199}"
[[ "${relay_port}" =~ ^[0-9]+$ ]] && (( relay_port >= 1024 && relay_port <= 65535 )) ||
  e2e_fail 'REMOTE_DOCKER_E2E_RELAY_PORT must be an unused TCP port from 1024 through 65535'

cleanup() {
  docker rm -f "${container}" >/dev/null 2>&1 || true
  docker volume rm "${volume}" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

container_running() {
  [[ "$(docker inspect --format '{{.State.Running}}' "${container}" 2>/dev/null)" == 'true' ]]
}

relay_ready() {
  [[ "$(curl --fail --silent --show-error --max-time 3 "http://127.0.0.1:${relay_port}/" 2>/dev/null)" == 'remote-docker-relay' ]]
}

docker volume create --label "io.github.dmitbd.remote-docker.e2e=${run_id}" "${volume}" >/dev/null
docker run -d --name "${container}" --restart always \
  --label "io.github.dmitbd.remote-docker.e2e=${run_id}" \
  --publish "127.0.0.1:${relay_port}:8080" \
  --mount "type=volume,src=${volume},dst=/state" "${image}" \
  sh -c 'test -f /state/sentinel || printf preserved >/state/sentinel; while true; do printf "HTTP/1.1 200 OK\r\nContent-Length: 19\r\nConnection: close\r\n\r\nremote-docker-relay" | nc -l -p 8080; done' >/dev/null
e2e_wait_until 30 'initial localhost relay' relay_ready

[[ -t 0 ]] || e2e_fail 'reconnect acceptance is interactive and requires a terminal'
e2e_note 'Temporarily disconnect the Mac from Wi-Fi, reconnect it to the same private network, then press Return.'
read -r _
e2e_wait_until "${REMOTE_DOCKER_E2E_RECONNECT_TIMEOUT:-180}" 'Docker recovery after Wi-Fi interruption' docker info
e2e_wait_until "${REMOTE_DOCKER_E2E_RECONNECT_TIMEOUT:-180}" 'localhost relay after Wi-Fi interruption' relay_ready
[[ "$(docker exec "${container}" cat /state/sentinel)" == 'preserved' ]] || e2e_fail 'named-volume state was lost after network recovery'

e2e_note 'Restart the Windows PC, wait until its desktop and Remote Docker tray are available, then type REBOOTED.'
read -r REMOTE_DOCKER_E2E_REBOOT_CONFIRMED
[[ "${REMOTE_DOCKER_E2E_REBOOT_CONFIRMED}" == 'REBOOTED' ]] || e2e_fail 'Windows reboot was not explicitly confirmed'
e2e_wait_until "${REMOTE_DOCKER_E2E_RECONNECT_TIMEOUT:-300}" 'Docker recovery after Windows restart' docker info
e2e_wait_until "${REMOTE_DOCKER_E2E_RECONNECT_TIMEOUT:-300}" 'restart-policy container recovery' \
  container_running
e2e_wait_until "${REMOTE_DOCKER_E2E_RECONNECT_TIMEOUT:-300}" 'localhost relay after Windows restart' relay_ready
[[ "$(docker exec "${container}" cat /state/sentinel)" == 'preserved' ]] || e2e_fail 'named-volume state was lost after Windows restart'

e2e_note 'Wi-Fi and Windows restart recovery: PASS'
