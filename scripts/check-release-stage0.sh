#!/usr/bin/env bash
# Compatibility entrypoint; use check-release-bootstrap.sh.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec "${ROOT}/scripts/check-release-bootstrap.sh" "$@"
