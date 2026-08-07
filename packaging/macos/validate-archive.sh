#!/bin/bash
set -euo pipefail

[[ $# -eq 3 ]] || {
  printf '%s\n' "usage: validate-archive.sh tar|zip ARCHIVE EXPECTED_ROOT" >&2
  exit 2
}

kind="$1"
archive="$2"
expected_root="$3"
[[ "${expected_root}" =~ ^[A-Za-z0-9._+-]+$ ]] || {
  printf '%s\n' "invalid expected archive root" >&2
  exit 1
}

member_file="$(mktemp "${TMPDIR:-/private/tmp}/remote-docker-archive-members.XXXXXX")"
mode_file="$(mktemp "${TMPDIR:-/private/tmp}/remote-docker-archive-modes.XXXXXX")"
trap 'rm -f -- "${member_file}" "${mode_file}"' EXIT

case "${kind}" in
  tar)
    LC_ALL=C tar -tzf "${archive}" >"${member_file}"
    LC_ALL=C tar -tvzf "${archive}" | awk '{ print substr($1, 1, 1) }' >"${mode_file}"
    ;;
  zip)
    unzip -Z1 "${archive}" >"${member_file}"
    unzip -Z -l "${archive}" | awk '$1 ~ /^[bcdlps-]/ { print substr($1, 1, 1) }' >"${mode_file}"
    ;;
  *)
    printf '%s\n' "unsupported archive type: ${kind}" >&2
    exit 2
    ;;
esac

[[ -s "${member_file}" && -s "${mode_file}" ]] || {
  printf '%s\n' "archive is empty or unreadable: ${archive}" >&2
  exit 1
}

while IFS= read -r member; do
  [[ -n "${member}" ]] || continue
  case "${member}" in
    /*|../*|*/../*|*/..)
      printf '%s\n' "archive contains an unsafe path: ${member}" >&2
      exit 1
      ;;
    "${expected_root}"|"${expected_root}"/*) ;;
    *)
      printf '%s\n' "archive member escapes expected root ${expected_root}: ${member}" >&2
      exit 1
      ;;
  esac
done <"${member_file}"

if grep -Ev '^[-d]$' "${mode_file}" >/dev/null; then
  printf '%s\n' "archive contains a link or special entry: ${archive}" >&2
  exit 1
fi
