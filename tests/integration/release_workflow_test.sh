#!/bin/bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
workflow="${repo_root}/.github/workflows/release.yml"
ci_workflow="${repo_root}/.github/workflows/ci.yml"

fail() {
  printf 'release workflow contract failed: %s\n' "$*" >&2
  exit 1
}

[[ -f "${workflow}" ]] || fail "workflow is missing"
[[ -f "${ci_workflow}" ]] || fail "CI workflow is missing"

for forbidden in \
  'WINDOWS_SIGNING_CERTIFICATE' \
  'MACOS_SIGNING_CERTIFICATE' \
  'REMOTE_DOCKER_APP_SIGN_IDENTITY' \
  'REMOTE_DOCKER_INSTALLER_SIGN_IDENTITY' \
  'APPLE_NOTARY_' \
  'verification.verified' \
  'signed-windows-release' \
  'signed-macos-release'; do
  if grep -F "${forbidden}" "${workflow}" >/dev/null; then
    fail "paid signing or signed-artifact requirement remains: ${forbidden}"
  fi
done

for required in \
  'remote-docker-windows-x64' \
  'remote-docker-macos-arm64' \
  'source_commit' \
  'GITHUB_SHA' \
  'Get-AuthenticodeSignature' \
  "Status -ne 'NotSigned'" \
  'remote-docker-windows.cdx.json' \
  'remote-docker-macos.cdx.json' \
  'Remote-Docker-Windows-x64-manifest.json' \
  'Remote-Docker-macOS-arm64-manifest.json' \
  'Remote-Docker-Windows-x64-SHA256SUMS' \
  'Remote-Docker-macOS-arm64-SHA256SUMS'; do
  grep -F "${required}" "${workflow}" >/dev/null || fail "missing release evidence: ${required}"
done

[[ "$(grep -c 'uses: actions/upload-artifact@' "${workflow}")" -ge 3 ]] || fail "expected rootfs and two desktop uploads"
grep -F 'gh release create "$GITHUB_REF_NAME"' "${workflow}" >/dev/null || fail "release publication is missing"

for required in \
  'runs-on: macos-14' \
  'bash tests/integration/macos_package_test.sh' \
  'packaging/macos/build-pkg.sh' \
  'packaging/macos/inspect-pkg.sh' \
  'remote-docker-macos-unsigned'; do
  grep -F "${required}" "${ci_workflow}" >/dev/null || fail "missing macOS CI evidence: ${required}"
done

printf 'release workflow contract: PASS\n'
