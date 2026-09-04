#!/usr/bin/env bash
set -euo pipefail

if (($# != 2)); then
  echo "usage: verify-bootstrap-endpoint.sh <https-url> <expected-source-file>" >&2
  exit 2
fi

url="$1"
source_file="$2"
[[ "${url}" == https://* ]] || {
  echo "bootstrap endpoint URL must use HTTPS" >&2
  exit 2
}
[[ -f "${source_file}" && ! -L "${source_file}" ]] || {
  echo "expected source must be a regular, non-symlink file" >&2
  exit 2
}

temporary="$(mktemp -d)"
trap 'rm -rf -- "${temporary}"' EXIT
trap 'exit 1' HUP INT TERM
downloaded="${temporary}/bootstrap"
if command -v curl >/dev/null 2>&1; then
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
    --output "${downloaded}" "${url}"
elif command -v wget >/dev/null 2>&1; then
  wget --https-only --secure-protocol=TLSv1_2 --quiet --output-document="${downloaded}" "${url}"
else
  echo "curl or wget is required" >&2
  exit 1
fi

cmp -- "${source_file}" "${downloaded}" || {
  echo "deployed bootstrap bytes differ from ${source_file}" >&2
  exit 1
}
sha256sum "${downloaded}"
