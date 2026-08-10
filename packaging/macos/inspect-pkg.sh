#!/bin/bash
set -euo pipefail

[[ $# -eq 1 || $# -eq 3 ]] || {
  printf '%s\n' "usage: inspect-pkg.sh PACKAGE [EXPECTED_PRODUCT_VERSION EXPECTED_BUILD_VERSION]" >&2
  exit 2
}

package_path="$1"
expected_product_version="${2:-}"
expected_build_version="${3:-}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -n "${expected_product_version}" ]]; then
  "${script_dir}/verify-package-version.sh" "${package_path}" "${expected_product_version}" "${expected_build_version}"
fi
inspection_root="$(mktemp -d "${TMPDIR:-/private/tmp}/remote-docker-pkg-inspect.XXXXXX")"
trap 'rm -rf -- "${inspection_root}"' EXIT

pkgutil --expand-full "${package_path}" "${inspection_root}/expanded"
bom="${inspection_root}/expanded/Bom"
[[ -f "${bom}" ]] || {
  printf '%s\n' "package has no inspectable BOM" >&2
  exit 1
}

if lsbom -f "${bom}" | awk '{ print $1 }' | grep -E '(^|/)\._|(^|/)\.DS_Store$' >/dev/null; then
  printf '%s\n' "package BOM contains AppleDouble or Finder metadata" >&2
  exit 1
fi
for tree in "${inspection_root}/expanded/Payload" "${inspection_root}/expanded/Scripts"; do
  [[ -e "${tree}" ]] || continue
  if find "${tree}" \( -name '._*' -o -name '.DS_Store' \) -print -quit | grep . >/dev/null; then
    printf '%s\n' "expanded package contains AppleDouble or Finder metadata" >&2
    exit 1
  fi
done

payload_root="${inspection_root}/expanded/Payload"
app_contents="${payload_root}/Applications/Remote Docker.app/Contents"
app_executable="${app_contents}/MacOS/remote-docker-desktop"
[[ -x "${app_executable}" ]] || {
  printf '%s\n' "package has no executable Remote Docker desktop application" >&2
  exit 1
}
[[ -f "${app_contents}/Resources/remote-docker.icns" ]] || {
  printf '%s\n' "package has no application icon" >&2
  exit 1
}
if find "${payload_root}" -path '*/Library/LoginItems/*' -o -path '*/Library/LaunchAgents/*' | grep . >/dev/null; then
  printf '%s\n' "package contains an automatic-start helper" >&2
  exit 1
fi
if ! dwarfdump --uuid "${app_executable}" | grep -E '^UUID: [0-9A-F-]+ \(' >/dev/null; then
  printf '%s\n' "macOS application has no Mach-O UUID for Local Network policy" >&2
  exit 1
fi
