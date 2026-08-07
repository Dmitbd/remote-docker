#!/bin/bash
set -euo pipefail

[[ $# -eq 3 ]] || {
  printf '%s\n' "usage: verify-checksum.sh CHECKSUMS FILE FILENAME" >&2
  exit 2
}

checksums_file="$1"
download_path="$2"
filename="$3"
expected="$(awk -v name="${filename}" '$2 == name { print $1 }' "${checksums_file}")"
[[ "${expected}" =~ ^[0-9a-f]{64}$ ]] || {
  printf 'missing checksum for %s\n' "${filename}" >&2
  exit 1
}
actual="$(shasum -a 256 "${download_path}" | awk '{ print $1 }')"
[[ "${actual}" == "${expected}" ]] || {
  printf 'checksum mismatch for %s\n' "${filename}" >&2
  exit 1
}
