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

require_text README.md 'git clone --branch vX.Y.Z --single-branch --depth 1'
require_text README.md 'CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE'
reject_text README.md '| bash -s'
reject_text docs/getting-started/production.md '| bash -s'
reject_text docs/getting-started/managed-node.md '| bash -s'
require_text docs/getting-started/production.md 'Stage-0 -> exact vX.Y.Z Stage-1 -> install.env -> durable clean checkout'
require_text docs/getting-started/managed-node.md 'Stage-0 -> exact vX.Y.Z Stage-1 -> signed checksum -> .deb/.rpm'
require_text docs/how-to/enroll-node.md 'grant `node.approve`'
require_text docs/operations/bootstrap-hosting.md 'test "$(tail -n 1 "$stage0")" = '\''main "$@"'\'' || exit 1'
require_text docs/operations/bootstrap-hosting.md 'Stage-0 is not a long-term upgrade, rollback, uninstall, or service manager.'
require_text docs/acceptance/bootstrap-install-closeout.md '## Supply-chain acceptance'

"${ROOT}/scripts/check-g6-contracts.mjs"
"${ROOT}/scripts/generate-g6-test-fixtures.mjs" --check
"${ROOT}/scripts/test-g6-evidence-verifier.mjs"
"${ROOT}/scripts/test-g6-evidence-builder.mjs"
"${ROOT}/scripts/test-g6-pipeline.mjs"
