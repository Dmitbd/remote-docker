#!/bin/bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
build_script="${repo_root}/packaging/macos/build-pkg.sh"
archive_validator="${repo_root}/packaging/macos/validate-archive.sh"
checksum_verifier="${repo_root}/packaging/macos/verify-checksum.sh"
package_inspector="${repo_root}/packaging/macos/inspect-pkg.sh"
test_root="$(mktemp -d "${TMPDIR:-/private/tmp}/remote-docker-package-test.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT

fail() {
  printf 'macOS package contract failed: %s\n' "$*" >&2
  exit 1
}

assert_file() {
  [[ -f "$1" ]] || fail "missing file $1"
}

assert_executable() {
  assert_file "$1"
  [[ -x "$1" ]] || fail "not executable: $1"
  [[ "$(stat -f '%Lp' "$1")" == "755" ]] || fail "unexpected mode for $1"
}

assert_plist_value() {
  local file="$1" key="$2" expected="$3" actual
  actual="$(/usr/bin/plutil -extract "${key}" raw -o - "${file}")"
  [[ "${actual}" == "${expected}" ]] || fail "${file}:${key}=${actual}, want ${expected}"
}

[[ -x "${build_script}" ]] || fail "missing executable build script"
[[ -x "${archive_validator}" ]] || fail "missing executable archive validator"
[[ -x "${checksum_verifier}" ]] || fail "missing executable checksum verifier"
[[ -x "${package_inspector}" ]] || fail "missing executable package metadata inspector"

export REMOTE_DOCKER_APP_SIGN_IDENTITY="PACKAGE_TEST_SECRET_APP_IDENTITY"
export REMOTE_DOCKER_INSTALLER_SIGN_IDENTITY="PACKAGE_TEST_SECRET_INSTALLER_IDENTITY"
export REMOTE_DOCKER_NOTARY_KEYCHAIN_PROFILE="PACKAGE_TEST_SECRET_NOTARY_PROFILE"
"${build_script}" --layout-only "${test_root}/layout" --arch arm64

payload="${test_root}/layout/payload"
scripts="${test_root}/layout/scripts"
app="${payload}/Applications/Remote Docker.app"
libexec="${payload}/usr/local/libexec/remote-docker"

assert_executable "${app}/Contents/MacOS/remote-docker-tray"
assert_executable "${app}/Contents/Resources/remote-docker-agent"
assert_executable "${payload}/usr/local/bin/remote-docker"
assert_executable "${libexec}/bin/docker"
assert_executable "${libexec}/docker-real"
assert_executable "${libexec}/cli-plugins/docker-compose"
assert_executable "${libexec}/syncthing"
assert_executable "${libexec}/uninstall"
assert_executable "${app}/Contents/libexec/remote-docker/docker-real"
assert_file "${payload}/Library/LaunchAgents/io.github.dmitbd.remote-docker.agent.plist"
assert_file "${payload}/etc/paths.d/remote-docker"

/usr/bin/plutil -lint "${app}/Contents/Info.plist" >/dev/null
assert_plist_value "${app}/Contents/Info.plist" CFBundleIdentifier io.github.dmitbd.remote-docker
assert_plist_value "${app}/Contents/Info.plist" CFBundleExecutable remote-docker-tray
assert_plist_value "${app}/Contents/Info.plist" LSUIElement true

launch_agent="${payload}/Library/LaunchAgents/io.github.dmitbd.remote-docker.agent.plist"
/usr/bin/plutil -lint "${launch_agent}" >/dev/null
assert_plist_value "${launch_agent}" Label io.github.dmitbd.remote-docker.agent
assert_plist_value "${launch_agent}" ProgramArguments.0 "/Applications/Remote Docker.app/Contents/Resources/remote-docker-agent"
assert_plist_value "${launch_agent}" RunAtLoad true
if /usr/bin/plutil -extract UserName raw -o - "${launch_agent}" >/dev/null 2>&1; then
  fail "LaunchAgent must inherit the logged-in user instead of naming root or another account"
fi

path_entry="$(sed -n '1p' "${payload}/etc/paths.d/remote-docker")"
[[ "${path_entry}" == "/usr/local/libexec/remote-docker/bin" ]] || fail "managed Docker launcher is not first in its PATH fragment"
[[ "$(wc -l < "${payload}/etc/paths.d/remote-docker" | tr -d ' ')" == "1" ]] || fail "PATH fragment owns unrelated entries"

docker_launcher="${libexec}/bin/docker"
grep -F 'exec /usr/local/bin/remote-docker docker "$@"' "${docker_launcher}" >/dev/null || fail "docker launcher does not delegate to the packaged Remote Docker Docker mode"
if grep -E 'exec[[:space:]]+(/usr/local/libexec/remote-docker/bin/docker|docker)([[:space:]]|$)' "${docker_launcher}" >/dev/null; then
  fail "docker launcher can recursively invoke itself"
fi
compose_link="${payload}/usr/local/libexec/docker/cli-plugins/docker-compose"
[[ -L "${compose_link}" ]] || fail "packaged Docker CLI cannot discover Compose through a standard plugin directory"
[[ "$(readlink "${compose_link}")" == "/usr/local/libexec/remote-docker/cli-plugins/docker-compose" ]] || fail "Compose discovery link escapes the owned plugin"

[[ ! -e "${payload}/usr/local/bin/docker" ]] || fail "package payload targets an existing docker command"
if find "${scripts}" -type f -exec grep -E '(^|[[:space:]])(rm|unlink)[[:space:]].*(\.docker|Library/(Application Support|Caches)/RemoteDocker)' {} + | grep .; then
  fail "package scripts can remove Docker data or user Remote Docker state"
fi
if grep -E '^[[:space:]]*(rm|unlink)[[:space:]].*(\$HOME|\$\{|[*?]|\.docker|Library/(Application Support|Caches)/RemoteDocker)' "${libexec}/uninstall" | grep -Fv 'rm -f "${docker_link}"'; then
  fail "uninstall helper contains a broad, variable, or state-destroying target"
fi
grep -F 'preserve /usr/local/bin/docker' "${libexec}/uninstall" >/dev/null || fail "uninstall contract does not explicitly preserve an existing docker command"
grep -F 'preserve pairing and workspace state' "${libexec}/uninstall" >/dev/null || fail "uninstall contract does not explicitly preserve user state"
grep -F 'ln -s "${managed_docker}" "${docker_link}"' "${scripts}/postinstall" >/dev/null || fail "postinstall does not create the managed docker command atomically"
for guard in "${scripts}/preinstall" "${scripts}/postinstall" "${libexec}/uninstall"; do
  grep -F '[ "$(readlink "${docker_link}")"' "${guard}" >/dev/null || fail "${guard} does not verify ownership of /usr/local/bin/docker"
done
grep -F 'rm -f "${docker_link}"' "${libexec}/uninstall" >/dev/null || fail "uninstall cannot remove its own exact managed docker link"

/usr/bin/ruby -rjson -e 'JSON.parse(File.read(ARGV.fetch(0)))' "${repo_root}/packaging/versions.json"
for key in go docker_cli compose syncthing; do
  for arch in amd64 arm64; do
    filename="$(/usr/bin/ruby -rjson -e 'data = JSON.parse(File.read(ARGV.fetch(0))); puts data.fetch("assets").fetch(ARGV.fetch(1)).fetch(ARGV.fetch(2)).fetch("filename")' "${repo_root}/packaging/versions.json" "${key}" "${arch}")"
    url="$(/usr/bin/ruby -rjson -e 'data = JSON.parse(File.read(ARGV.fetch(0))); puts data.fetch("assets").fetch(ARGV.fetch(1)).fetch(ARGV.fetch(2)).fetch("url")' "${repo_root}/packaging/versions.json" "${key}" "${arch}")"
    [[ "${url}" == https://* ]] || fail "${key}/${arch} does not use HTTPS"
    [[ "$(grep -Ec "^[0-9a-f]{64}  ${filename}$" "${repo_root}/packaging/checksums.txt")" == "1" ]] || fail "${filename} is not pinned exactly once"
  done
done
grep -F 'verify-checksum.sh" "${checksums_file}" "${download_path}" "${filename}"' "${build_script}" >/dev/null || fail "downloads are not verified before use"
grep -F 'xattr -crs "${payload}"' "${build_script}" >/dev/null || fail "payload xattrs are not cleared without following package symlinks"
grep -F -- "--filter '(^|/)\\._'" "${build_script}" >/dev/null || fail "pkgbuild does not exclude AppleDouble metadata"
for extractor in 'tar -x' 'unzip -q'; do
  grep -F "${extractor}" "${build_script}" >/dev/null || fail "build script does not handle expected archive type"
done

mkdir -p "${test_root}/safe-archive/go" "${test_root}/unsafe-archive/go"
printf '%s\n' safe >"${test_root}/safe-archive/go/tool"
tar -czf "${test_root}/safe.tar.gz" -C "${test_root}/safe-archive" go
"${archive_validator}" tar "${test_root}/safe.tar.gz" go
ln -s /private/tmp "${test_root}/unsafe-archive/go/escape"
tar -czf "${test_root}/unsafe.tar.gz" -C "${test_root}/unsafe-archive" go
if "${archive_validator}" tar "${test_root}/unsafe.tar.gz" go >/dev/null 2>&1; then
  fail "archive validator accepted a symlink entry"
fi

printf '%s\n' corrupted >"${test_root}/go1.26.5.darwin-arm64.tar.gz"
if "${checksum_verifier}" "${repo_root}/packaging/checksums.txt" \
  "${test_root}/go1.26.5.darwin-arm64.tar.gz" go1.26.5.darwin-arm64.tar.gz >/dev/null 2>&1; then
  fail "checksum verifier accepted a corrupt cached artifact"
fi

mkdir -p "${test_root}/dirty-package-root"
printf '%s\n' forbidden >"${test_root}/dirty-package-root/._forbidden"
pkgbuild --root "${test_root}/dirty-package-root" \
  --identifier io.github.dmitbd.remote-docker.metadata-test --version 0 --install-location / \
  "${test_root}/dirty.pkg" >/dev/null
if "${package_inspector}" "${test_root}/dirty.pkg" >/dev/null 2>&1; then
  fail "package metadata inspector accepted an AppleDouble BOM entry"
fi
grep -F 'refusing signed or notarized package with metadata contamination' "${build_script}" >/dev/null || fail "release build does not fail closed on metadata contamination"

if grep -R -F 'PACKAGE_TEST_SECRET_' "${test_root}/layout" >/dev/null; then
  fail "signing or notarization secret was persisted in package layout"
fi
grep -F 'set -x' "${build_script}" >/dev/null && fail "build script can echo signing secrets"
if grep -R -E '/Applications/Docker\.app|dockerd([[:space:]]|$)' "${repo_root}/packaging/macos" "${scripts}" >/dev/null; then
  fail "macOS package downloads, installs, or starts Docker Desktop/a local daemon"
fi

public_files="${test_root}/public-files.txt"
git -C "${repo_root}" ls-files --cached --others --exclude-standard >"${public_files}"
internal_pattern='S''ber|Co''work|Mid''gard|Ygg''drasil'
if tr '\n' '\0' <"${public_files}" | xargs -0 grep -I -n -E "${internal_pattern}" 2>/dev/null; then
  fail "public repository boundary contains an internal or corporate name"
fi
if grep -E '(^|/)(superpowers|specs?|agent-reports?)(/|$)' "${public_files}" >/dev/null; then
  fail "internal task artifacts were added to the public repository"
fi

printf 'macOS package contract: PASS\n'
