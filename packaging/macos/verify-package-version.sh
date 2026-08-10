#!/bin/bash
set -euo pipefail

[[ $# -eq 3 ]] || {
  printf '%s\n' "usage: verify-package-version.sh PACKAGE EXPECTED_PRODUCT_VERSION EXPECTED_BUILD_VERSION" >&2
  exit 2
}

package_path="$1"
expected_product_version="$2"
expected_build_version="$3"
inspection_root="$(mktemp -d "${TMPDIR:-/private/tmp}/remote-docker-version-inspect.XXXXXX")"
trap 'rm -rf -- "${inspection_root}"' EXIT

pkgutil --expand-full "${package_path}" "${inspection_root}/expanded"
package_info="${inspection_root}/expanded/PackageInfo"
app_plist="${inspection_root}/expanded/Payload/Applications/Remote Docker.app/Contents/Info.plist"
[[ -f "${package_info}" ]] || {
  printf '%s\n' "package has no inspectable package version" >&2
  exit 1
}
[[ -f "${app_plist}" ]] || {
  printf '%s\n' "package has no inspectable application version" >&2
  exit 1
}

package_version="$(/usr/bin/ruby -rrexml/document -e 'puts REXML::Document.new(File.read(ARGV.fetch(0))).root.attributes["version"]' "${package_info}")"
app_product_version="$(/usr/bin/plutil -extract CFBundleShortVersionString raw -o - "${app_plist}")"
app_build_version="$(/usr/bin/plutil -extract CFBundleVersion raw -o - "${app_plist}")"

[[ "${package_version}" == "${expected_product_version}" ]] || {
  printf 'package version %s does not match expected product version %s\n' "${package_version}" "${expected_product_version}" >&2
  exit 1
}
[[ "${app_product_version}" == "${expected_product_version}" ]] || {
  printf 'application product version %s does not match expected product version %s\n' "${app_product_version}" "${expected_product_version}" >&2
  exit 1
}
[[ "${app_build_version}" == "${expected_build_version}" ]] || {
  printf 'application build version %s does not match expected build version %s\n' "${app_build_version}" "${expected_build_version}" >&2
  exit 1
}
