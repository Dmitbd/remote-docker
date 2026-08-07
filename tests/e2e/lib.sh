#!/usr/bin/env bash
set -euo pipefail

e2e_note() {
  printf '[remote-docker-e2e] %s\n' "$*"
}

e2e_fail() {
  printf '[remote-docker-e2e] FAIL: %s\n' "$*" >&2
  exit 1
}

e2e_require_command() {
  command -v "$1" >/dev/null 2>&1 || e2e_fail "required command is unavailable: $1"
}

e2e_require_opt_in() {
  [[ "${REMOTE_DOCKER_E2E_CONFIRM:-}" == 'I_UNDERSTAND_THIS_CREATES_TEST_RESOURCES' ]] ||
    e2e_fail 'set REMOTE_DOCKER_E2E_CONFIRM=I_UNDERSTAND_THIS_CREATES_TEST_RESOURCES to run acceptance tests'
}

e2e_assert_remote_engine() {
  e2e_require_command docker
  e2e_require_command remote-docker

  local status context engine
  status="$(remote-docker status --json)" || e2e_fail 'the background agent is unavailable'
  printf '%s' "${status}" | grep -Eq '"state"[[:space:]]*:[[:space:]]*"Ready"' ||
    e2e_fail 'the background agent is not Ready'

  context="$(docker context inspect remote-docker --format '{{.Metadata.Description}}|{{(index .Endpoints "docker").Host}}')" ||
    e2e_fail 'the managed Docker context is unavailable'
  case "${context}" in
    'Managed by Remote Docker|ssh://remote-docker-device-'*) ;;
    *) e2e_fail "refusing to test an unmanaged Docker context: ${context}" ;;
  esac

  [[ "$(docker context show)" == 'remote-docker' ]] || e2e_fail 'Docker is not using the remote-docker context'
  engine="$(docker info --format '{{.Name}}|{{.OSType}}|{{.Architecture}}|{{.DockerRootDir}}')" ||
    e2e_fail 'the remote Docker Engine is unavailable'
  case "${engine}" in
    *'|linux|'*'|/'*) ;;
    *) e2e_fail "unexpected remote Engine identity: ${engine}" ;;
  esac
  e2e_note "verified remote Engine: ${engine}"
}

e2e_require_workspace() {
  local workspace="${REMOTE_DOCKER_E2E_WORKSPACE:-}"
  [[ -n "${workspace}" && -d "${workspace}" ]] ||
    e2e_fail 'REMOTE_DOCKER_E2E_WORKSPACE must name an existing registered source directory'
  workspace="$(cd "${workspace}" && pwd -P)"
  remote-docker workspace add "${workspace}" >/dev/null || e2e_fail 'cannot register the acceptance workspace'
  printf '%s\n' "${workspace}"
}

e2e_new_id() {
  printf 'rd-e2e-%s-%s\n' "$(date +%Y%m%d%H%M%S)" "$$"
}

e2e_wait_until() {
  local timeout="$1"
  local description="$2"
  shift 2
  local started
  started="$(date +%s)"
  while ! "$@"; do
    if (( $(date +%s) - started >= timeout )); then
      e2e_fail "timed out waiting for ${description}"
    fi
    sleep 1
  done
}

e2e_assert_owned_temp() {
  local workspace="$1"
  local path="$2"
  case "${path}" in
    "${workspace}"/.remote-docker-e2e-*) ;;
    *) e2e_fail "refusing to clean an unexpected path: ${path}" ;;
  esac
}
