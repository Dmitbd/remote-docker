#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/e2e/lib.sh
source "${script_dir}/lib.sh"

e2e_require_opt_in
e2e_assert_remote_engine
workspace="$(e2e_require_workspace)"
run_id="$(e2e_new_id)"
image="${REMOTE_DOCKER_E2E_IMAGE:-alpine:3.21}"
container="${run_id}-container"
network="${run_id}-network"
volume="${run_id}-volume"
project="${run_id//-/_}"
case_dir="$(mktemp -d "${workspace}/.remote-docker-e2e-compat.XXXXXX")"
e2e_assert_owned_temp "${workspace}" "${case_dir}"

cleanup() {
  docker compose --project-name "${project}" --project-directory "${case_dir}" down --volumes --remove-orphans >/dev/null 2>&1 || true
  docker rm -f "${container}" >/dev/null 2>&1 || true
  docker network rm "${network}" >/dev/null 2>&1 || true
  docker volume rm "${volume}" >/dev/null 2>&1 || true
  docker image rm "${run_id}:latest" >/dev/null 2>&1 || true
  e2e_assert_owned_temp "${workspace}" "${case_dir}"
  rm -rf "${case_dir}"
}
trap cleanup EXIT INT TERM

cat >"${case_dir}/Dockerfile" <<EOF
ARG BASE_IMAGE=${image}
FROM \${BASE_IMAGE}
WORKDIR /workspace
COPY payload.txt /image-payload.txt
CMD ["sh", "-c", "sleep 300"]
EOF
printf 'remote-docker compatibility payload\n' >"${case_dir}/payload.txt"
cat >"${case_dir}/compose.yaml" <<EOF
services:
  app:
    image: ${run_id}:latest
    build:
      context: .
      args:
        BASE_IMAGE: ${image}
    command: ["sh", "-c", "sleep 300"]
    volumes:
      - .:/workspace
  pullcheck:
    image: ${image}
    profiles: ["pullcheck"]
    command: ["true"]
EOF

docker version >/dev/null
docker info >/dev/null
docker ps >/dev/null
docker images >/dev/null
docker network ls >/dev/null
docker volume ls >/dev/null
docker pull "${image}" >/dev/null
docker network create --label "io.github.dmitbd.remote-docker.e2e=${run_id}" "${network}" >/dev/null
docker volume create --label "io.github.dmitbd.remote-docker.e2e=${run_id}" "${volume}" >/dev/null
docker build --label "io.github.dmitbd.remote-docker.e2e=${run_id}" -t "${run_id}:latest" "${case_dir}" >/dev/null
docker run -d --name "${container}" --label "io.github.dmitbd.remote-docker.e2e=${run_id}" \
  --network "${network}" --mount "type=volume,src=${volume},dst=/data" "${run_id}:latest" >/dev/null
docker exec "${container}" sh -c 'printf ok >/data/exec.txt'
[[ "$(docker exec "${container}" cat /data/exec.txt)" == 'ok' ]]
docker logs "${container}" >/dev/null
docker stats --no-stream "${container}" >/dev/null
docker cp "${container}:/image-payload.txt" "${case_dir}/copied-from-container.txt"
grep -q 'remote-docker compatibility payload' "${case_dir}/copied-from-container.txt"

docker compose --project-name "${project}" --project-directory "${case_dir}" build >/dev/null
docker compose --project-name "${project}" --project-directory "${case_dir}" up -d app >/dev/null
docker compose --project-name "${project}" --project-directory "${case_dir}" exec -T app test -f /workspace/payload.txt
docker compose --project-name "${project}" --project-directory "${case_dir}" run --rm app test -f /image-payload.txt
docker compose --project-name "${project}" --project-directory "${case_dir}" --profile pullcheck pull pullcheck >/dev/null
docker compose --project-name "${project}" --project-directory "${case_dir}" down --volumes --remove-orphans >/dev/null

e2e_note 'Docker and Docker Compose compatibility: PASS'
