#!/bin/bash
set -euo pipefail

[[ $# -eq 1 ]] || {
  printf '%s\n' "usage: inspect-pkg.sh PACKAGE" >&2
  exit 2
}

package_path="$1"
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

agent_app="${inspection_root}/expanded/Payload/Applications/Remote Docker.app/Contents/Library/LoginItems/Remote Docker Agent.app"
if [[ -d "${agent_app}" ]]; then
  agent_executable="${agent_app}/Contents/MacOS/remote-docker-agent"
  [[ -x "${agent_executable}" ]] || {
    printf '%s\n' "package has no executable macOS background agent" >&2
    exit 1
  }
  if ! dwarfdump --uuid "${agent_executable}" | grep -E '^UUID: [0-9A-F-]+ \(' >/dev/null; then
    printf '%s\n' "macOS background agent has no Mach-O UUID for Local Network policy" >&2
    exit 1
  fi
fi
