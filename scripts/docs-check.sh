#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

if git -C "${ROOT}" grep -I -n $'\r' -- '*.md' '*.yaml' '*.yml' '*.proto' '*.go' '*.rs' '*.ts'; then
  echo "CRLF line ending found" >&2
  exit 1
fi

while IFS= read -r file; do
  [[ -s "${ROOT}/${file}" ]] || {
    echo "empty public documentation file: ${file}" >&2
    exit 1
  }
done < <(git -C "${ROOT}" ls-files '*.md')

require_text() {
  local file="$1" text="$2"
  grep -Fq -- "${text}" "${ROOT}/${file}" || {
    echo "required bootstrap documentation is missing from ${file}: ${text}" >&2
    exit 1
  }
}

reject_text() {
  local file="$1" text="$2"
  if grep -Fq -- "${text}" "${ROOT}/${file}"; then
    echo "forbidden bootstrap documentation found in ${file}: ${text}" >&2
    exit 1
  fi
}

require_text README.md 'docs/getting-started/production.md'
reject_text README.md '| bash -s'
reject_text docs/getting-started/production.md '| bash -s'
reject_text docs/getting-started/managed-node.md '| bash -s'
