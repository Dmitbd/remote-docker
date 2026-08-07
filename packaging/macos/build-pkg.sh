#!/bin/bash
set -euo pipefail
export COPYFILE_DISABLE=1

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
versions_file="${repo_root}/packaging/versions.json"
checksums_file="${repo_root}/packaging/checksums.txt"
layout_only="false"
layout_output=""
output_dir="${repo_root}/dist"
target_arch=""

usage() {
  printf '%s\n' "usage: packaging/macos/build-pkg.sh [--arch arm64|amd64] [--output DIR]"
  printf '%s\n' "       packaging/macos/build-pkg.sh --layout-only DIR --arch arm64|amd64"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --arch)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      target_arch="$2"
      shift 2
      ;;
    --output)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      output_dir="$2"
      shift 2
      ;;
    --layout-only)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      layout_only="true"
      layout_output="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'unsupported argument: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

host_arch="$(uname -m)"
case "${host_arch}" in
  arm64) host_arch="arm64" ;;
  x86_64) host_arch="amd64" ;;
  *) printf 'unsupported macOS host architecture: %s\n' "${host_arch}" >&2; exit 1 ;;
esac
if [[ -z "${target_arch}" ]]; then
  target_arch="${host_arch}"
fi
case "${target_arch}" in
  arm64|amd64) ;;
  *) printf 'unsupported package architecture: %s\n' "${target_arch}" >&2; exit 1 ;;
esac
if [[ "${layout_only}" != "true" && "${target_arch}" != "${host_arch}" ]]; then
  printf 'cross-architecture tray builds are unsupported; build %s on a %s Mac\n' "${target_arch}" "${target_arch}" >&2
  exit 1
fi

app_version="${REMOTE_DOCKER_VERSION:-0.1.0}"
build_version="${REMOTE_DOCKER_BUILD_VERSION:-1}"
[[ "${app_version}" =~ ^[0-9][0-9A-Za-z.-]*$ ]] || { printf 'invalid REMOTE_DOCKER_VERSION\n' >&2; exit 1; }
[[ "${build_version}" =~ ^[0-9]+([.][0-9]+)*$ ]] || { printf 'invalid REMOTE_DOCKER_BUILD_VERSION\n' >&2; exit 1; }

private_tmp_parent="${TMPDIR:-/private/tmp}"
work_root="$(mktemp -d "${private_tmp_parent%/}/remote-docker-pkg.XXXXXX")"
cleanup() {
  case "${work_root}" in
    "${private_tmp_parent%/}"/remote-docker-pkg.*) rm -rf -- "${work_root}" ;;
    *) printf 'refusing to clean unexpected staging path: %s\n' "${work_root}" >&2 ;;
  esac
}
trap cleanup EXIT

stage_root="${work_root}/layout"
payload="${stage_root}/payload"
package_scripts="${stage_root}/scripts"
app_bundle="${payload}/Applications/Remote Docker.app"
app_contents="${app_bundle}/Contents"
libexec="${payload}/usr/local/libexec/remote-docker"

create_layout() {
  mkdir -p \
    "${app_contents}/MacOS" \
    "${app_contents}/Resources" \
    "${app_contents}/libexec/remote-docker" \
    "${payload}/Library/LaunchAgents" \
    "${payload}/usr/local/bin" \
    "${libexec}/bin" \
    "${libexec}/cli-plugins" \
    "${payload}/usr/local/libexec/docker/cli-plugins" \
    "${payload}/etc/paths.d" \
    "${package_scripts}"

  cp "${script_dir}/Info.plist" "${app_contents}/Info.plist"
  sed -i '' \
    -e "s/__REMOTE_DOCKER_VERSION__/${app_version}/g" \
    -e "s/__REMOTE_DOCKER_BUILD_VERSION__/${build_version}/g" \
    "${app_contents}/Info.plist"
  cp "${script_dir}/io.github.dmitbd.remote-docker.agent.plist" \
    "${payload}/Library/LaunchAgents/io.github.dmitbd.remote-docker.agent.plist"
  cp "${script_dir}/paths.d/remote-docker" "${payload}/etc/paths.d/remote-docker"
  cp "${script_dir}/templates/docker" "${libexec}/bin/docker"
  cp "${script_dir}/templates/uninstall" "${libexec}/uninstall"
  cp "${script_dir}/scripts/preinstall" "${package_scripts}/preinstall"
  cp "${script_dir}/scripts/postinstall" "${package_scripts}/postinstall"
  cp "${repo_root}/THIRD_PARTY_NOTICES.md" "${app_contents}/Resources/THIRD_PARTY_NOTICES.md"

  ln -s "/usr/local/libexec/remote-docker/cli-plugins/docker-compose" \
    "${payload}/usr/local/libexec/docker/cli-plugins/docker-compose"
  chmod 755 "${libexec}/bin/docker" "${libexec}/uninstall" "${package_scripts}/preinstall" "${package_scripts}/postinstall"
  chmod 644 \
    "${app_contents}/Info.plist" \
    "${payload}/Library/LaunchAgents/io.github.dmitbd.remote-docker.agent.plist" \
    "${payload}/etc/paths.d/remote-docker" \
    "${app_contents}/Resources/THIRD_PARTY_NOTICES.md"
}

write_layout_placeholder() {
  local destination="$1" label="$2"
  printf '#!/bin/sh\nprintf "%%s\\n" %q\n' "${label}" >"${destination}"
  chmod 755 "${destination}"
}

asset_value() {
  local asset="$1" field="$2"
  /usr/bin/ruby -rjson -e \
    'data = JSON.parse(File.read(ARGV.fetch(0))); puts data.fetch("assets").fetch(ARGV.fetch(1)).fetch(ARGV.fetch(2)).fetch(ARGV.fetch(3))' \
    "${versions_file}" "${asset}" "${target_arch}" "${field}"
}

fetch_verified() {
  local asset="$1" filename url download_path cache_root
  filename="$(asset_value "${asset}" filename)"
  url="$(asset_value "${asset}" url)"
  [[ "${url}" == https://* ]] || { printf 'refusing non-HTTPS asset URL\n' >&2; return 1; }
  download_path="${work_root}/downloads/${filename}"
  mkdir -p "${work_root}/downloads"
  cache_root="${REMOTE_DOCKER_ASSET_CACHE:-}"
  if [[ -n "${cache_root}" ]]; then
    [[ "${cache_root}" == /* ]] || { printf 'asset cache path must be absolute\n' >&2; return 1; }
    [[ ! -L "${cache_root}/${filename}" ]] || { printf 'asset cache entry must not be a symlink\n' >&2; return 1; }
  fi
  if [[ -n "${cache_root}" && -f "${cache_root}/${filename}" ]]; then
    cp "${cache_root}/${filename}" "${download_path}"
  else
    curl --proto '=https' --tlsv1.2 --fail --location --silent --show-error \
      --connect-timeout 20 --max-time 600 --retry 3 \
      "${url}" --output "${download_path}"
  fi
  "${script_dir}/verify-checksum.sh" "${checksums_file}" "${download_path}" "${filename}"
  if [[ -n "${cache_root}" && ! -e "${cache_root}/${filename}" ]]; then
    mkdir -p "${cache_root}"
    cp "${download_path}" "${cache_root}/${filename}"
  fi
  printf '%s\n' "${download_path}"
}

build_payload() {
  local go_archive docker_archive compose_binary syncthing_archive go_root syncthing_root syncthing_binary
  go_archive="$(fetch_verified go)"
  docker_archive="$(fetch_verified docker_cli)"
  compose_binary="$(fetch_verified compose)"
  syncthing_archive="$(fetch_verified syncthing)"

  "${script_dir}/validate-archive.sh" tar "${go_archive}" go
  "${script_dir}/validate-archive.sh" tar "${docker_archive}" docker
  "${script_dir}/validate-archive.sh" zip "${syncthing_archive}" "$(basename "${syncthing_archive}" .zip)"

  go_root="${work_root}/toolchain"
  mkdir -p "${go_root}" "${work_root}/docker" "${work_root}/syncthing"
  tar -xzf "${go_archive}" -C "${go_root}"
  tar -xzf "${docker_archive}" -C "${work_root}/docker"
  unzip -q "${syncthing_archive}" -d "${work_root}/syncthing"
  [[ "$("${go_root}/go/bin/go" version)" == go\ version\ go1.26.5\ darwin/* ]] || {
    printf 'downloaded Go toolchain has an unexpected version\n' >&2
    return 1
  }
  [[ -x "${work_root}/docker/docker/docker" ]] || { printf 'Docker archive lacks the CLI binary\n' >&2; return 1; }
  syncthing_root="${work_root}/syncthing"
  syncthing_binary="${syncthing_root}/$(basename "${syncthing_archive}" .zip)/syncthing"
  [[ -f "${syncthing_binary}" && -x "${syncthing_binary}" ]] || {
    printf 'Syncthing archive has an unexpected layout\n' >&2
    return 1
  }

  env \
    GOROOT="${go_root}/go" \
    PATH="${go_root}/go/bin:/usr/bin:/bin:/usr/sbin:/sbin" \
    GOOS=darwin GOARCH="${target_arch}" CGO_ENABLED=1 \
    "${go_root}/go/bin/go" -C "${repo_root}" build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' \
    -o "${payload}/usr/local/bin/remote-docker" ./cmd/remote-docker
  env \
    GOROOT="${go_root}/go" \
    PATH="${go_root}/go/bin:/usr/bin:/bin:/usr/sbin:/sbin" \
    GOOS=darwin GOARCH="${target_arch}" CGO_ENABLED=1 \
    "${go_root}/go/bin/go" -C "${repo_root}" build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' \
    -o "${app_contents}/Resources/remote-docker-agent" ./cmd/remote-docker-agent
  env \
    GOROOT="${go_root}/go" \
    PATH="${go_root}/go/bin:/usr/bin:/bin:/usr/sbin:/sbin" \
    GOOS=darwin GOARCH="${target_arch}" CGO_ENABLED=1 \
    "${go_root}/go/bin/go" -C "${repo_root}" build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' \
    -o "${app_contents}/MacOS/remote-docker-tray" ./cmd/remote-docker-tray

  cp "${work_root}/docker/docker/docker" "${libexec}/docker-real"
  cp "${work_root}/docker/docker/docker" "${app_contents}/libexec/remote-docker/docker-real"
  cp "${compose_binary}" "${libexec}/cli-plugins/docker-compose"
  cp "${syncthing_binary}" "${libexec}/syncthing"
  chmod 755 \
    "${payload}/usr/local/bin/remote-docker" \
    "${app_contents}/Resources/remote-docker-agent" \
    "${app_contents}/MacOS/remote-docker-tray" \
    "${app_contents}/libexec/remote-docker/docker-real" \
    "${libexec}/docker-real" \
    "${libexec}/cli-plugins/docker-compose" \
    "${libexec}/syncthing"
}

sign_app_if_requested() {
  local identity="${REMOTE_DOCKER_APP_SIGN_IDENTITY:-}"
  [[ -n "${identity}" ]] || return 0
  command -v codesign >/dev/null || { printf 'codesign is required for app signing\n' >&2; return 1; }
  local target
  for target in \
    "${payload}/usr/local/bin/remote-docker" \
    "${app_contents}/Resources/remote-docker-agent" \
    "${app_contents}/MacOS/remote-docker-tray" \
    "${app_contents}/libexec/remote-docker/docker-real" \
    "${libexec}/docker-real" \
    "${libexec}/cli-plugins/docker-compose" \
    "${libexec}/syncthing" \
    "${app_bundle}"; do
    if ! codesign --force --options runtime --timestamp --sign "${identity}" "${target}" >/dev/null 2>&1; then
      printf 'application signing failed for %s\n' "${target}" >&2
      return 1
    fi
  done
}

create_layout
if [[ "${layout_only}" == "true" ]]; then
  [[ -n "${layout_output}" ]] || { printf 'layout output is required\n' >&2; exit 2; }
  [[ ! -e "${layout_output}" ]] || { printf 'layout output already exists: %s\n' "${layout_output}" >&2; exit 1; }
  write_layout_placeholder "${payload}/usr/local/bin/remote-docker" "remote-docker layout placeholder"
  write_layout_placeholder "${app_contents}/Resources/remote-docker-agent" "agent layout placeholder"
  write_layout_placeholder "${app_contents}/MacOS/remote-docker-tray" "tray layout placeholder"
  write_layout_placeholder "${app_contents}/libexec/remote-docker/docker-real" "agent Docker CLI layout placeholder"
  write_layout_placeholder "${libexec}/docker-real" "Docker CLI layout placeholder"
  write_layout_placeholder "${libexec}/cli-plugins/docker-compose" "Compose layout placeholder"
  write_layout_placeholder "${libexec}/syncthing" "Syncthing layout placeholder"
  mkdir -p "$(dirname "${layout_output}")"
  cp -R "${stage_root}" "${layout_output}"
  exit 0
fi

[[ "$(uname -s)" == "Darwin" ]] || { printf 'macOS package builds require macOS\n' >&2; exit 1; }
for tool in curl tar unzip shasum pkgbuild pkgutil lsbom xattr /usr/bin/ruby; do
  command -v "${tool}" >/dev/null || { printf 'required build tool is missing: %s\n' "${tool}" >&2; exit 1; }
done

build_payload
xattr -crs "${payload}"
sign_app_if_requested
mkdir -p "${output_dir}"
unsigned_pkg="${work_root}/Remote-Docker-${app_version}-${target_arch}-unsigned.pkg"
pkgbuild \
  --root "${payload}" \
  --scripts "${package_scripts}" \
  --filter '(^|/)\._' \
  --filter '(^|/)\.DS_Store$' \
  --filter '(^|/)\.svn(/|$)' \
  --filter '(^|/)CVS(/|$)' \
  --identifier io.github.dmitbd.remote-docker \
  --version "${app_version}" \
  --install-location / \
  --ownership recommended \
  "${unsigned_pkg}"

if ! "${script_dir}/inspect-pkg.sh" "${unsigned_pkg}"; then
  if [[ -n "${REMOTE_DOCKER_APP_SIGN_IDENTITY:-}${REMOTE_DOCKER_INSTALLER_SIGN_IDENTITY:-}${REMOTE_DOCKER_NOTARY_KEYCHAIN_PROFILE:-}" ]]; then
    printf '%s\n' "refusing signed or notarized package with metadata contamination" >&2
    exit 1
  fi
  printf '%s\n' "warning: unsigned development package contains host provenance metadata; release signing is blocked" >&2
fi

final_pkg="${output_dir}/Remote-Docker-${app_version}-${target_arch}.pkg"
installer_identity="${REMOTE_DOCKER_INSTALLER_SIGN_IDENTITY:-}"
if [[ -n "${installer_identity}" ]]; then
  command -v productsign >/dev/null || { printf 'productsign is required for installer signing\n' >&2; exit 1; }
  if ! productsign --sign "${installer_identity}" "${unsigned_pkg}" "${final_pkg}" >/dev/null 2>&1; then
    printf 'installer signing failed\n' >&2
    exit 1
  fi
else
  cp "${unsigned_pkg}" "${final_pkg}"
fi

notary_profile="${REMOTE_DOCKER_NOTARY_KEYCHAIN_PROFILE:-}"
if [[ -n "${notary_profile}" ]]; then
  [[ -n "${installer_identity}" ]] || { printf 'notarization requires an installer signing identity\n' >&2; exit 1; }
  if ! xcrun notarytool submit "${final_pkg}" --keychain-profile "${notary_profile}" --wait >/dev/null 2>&1; then
    printf 'notarization failed\n' >&2
    exit 1
  fi
  if ! xcrun stapler staple "${final_pkg}" >/dev/null 2>&1; then
    printf 'notarization stapling failed\n' >&2
    exit 1
  fi
fi

printf 'created %s\n' "${final_pkg}"
