#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
"${ROOT}/scripts/lint.sh" common
"${ROOT}/scripts/check-breaking.sh"
"${ROOT}/scripts/go-check.sh" standard
"${ROOT}/scripts/rust-check.sh"
"${ROOT}/scripts/transport-boundary-check.sh"
"${ROOT}/scripts/agent-boundary-check.sh"
"${ROOT}/scripts/web-check.sh" basic
"${ROOT}/scripts/security-check.sh"
"${ROOT}/scripts/license-check.sh"
"${ROOT}/scripts/generated-clean.sh"
"${ROOT}/scripts/test-hygiene-check.sh"
