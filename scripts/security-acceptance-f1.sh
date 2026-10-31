#!/usr/bin/env bash
# Compatibility entrypoint; use security-acceptance-transport.sh.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec "${ROOT}/scripts/security-acceptance-transport.sh" "$@"
