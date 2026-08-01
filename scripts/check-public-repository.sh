#!/usr/bin/env bash
set -euo pipefail

ROOT="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

forbidden_path='(^|/)(AGENTS\.md|\.ocservia-control)(/|$)|(^|/)(implementation-tracker\.csv|EVIDENCE\.md)$|(^|/)(prompts?|threat-models?|internal-adrs?|buildserver-logs?)(/|$)'
tracked="$(git -C "${ROOT}" ls-files)"

if printf '%s\n' "${tracked}" | grep -E "${forbidden_path}"; then
  echo "tracked paths contain local implementation-control material" >&2
  exit 1
fi

if git -C "${ROOT}" grep -I -n -E '\.ocservia-control/|implementation-tracker\.csv' -- \
  ':!scripts/check-public-repository.sh' \
  ':!scripts/test-public-repository-policy.sh'; then
  echo "public files disclose local control paths" >&2
  exit 1
fi
