#!/usr/bin/env bash
# Compatibility entrypoint; use security-acceptance-sse-capacity.sh.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec "${ROOT}/scripts/security-acceptance-sse-capacity.sh" "$@"
