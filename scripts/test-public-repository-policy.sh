#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
temporary="$(mktemp -d)"
trap 'rm -rf "${temporary}"' EXIT INT TERM

git -C "${temporary}" init -q
mkdir -p "${temporary}/scripts"
cp "${ROOT}/scripts/check-public-repository.sh" "${temporary}/scripts/"
printf '%s\n' '# local control' >"${temporary}/AGENTS.md"
git -C "${temporary}" add -f AGENTS.md scripts/check-public-repository.sh

if "${temporary}/scripts/check-public-repository.sh" "${temporary}" >/dev/null 2>&1; then
  echo "policy check accepted forbidden AGENTS.md" >&2
  exit 1
fi

rm "${temporary}/AGENTS.md"
mkdir -p "${temporary}/.ocservia-control"
printf '%s\n' 'private' >"${temporary}/.ocservia-control/prompt.md"
git -C "${temporary}" add -Af

if "${temporary}/scripts/check-public-repository.sh" "${temporary}" >/dev/null 2>&1; then
  echo "policy check accepted forbidden control directory" >&2
  exit 1
fi

rm -rf "${temporary}/.ocservia-control"
mkdir -p "${temporary}/localserver-log"
printf '%s\n' 'private' >"${temporary}/localserver-log/run.log"
git -C "${temporary}" add -Af

if "${temporary}/scripts/check-public-repository.sh" "${temporary}" >/dev/null 2>&1; then
  echo "policy check accepted forbidden localserver-log directory" >&2
  exit 1
fi

rm -rf "${temporary}/localserver-log"
mkdir -p "${temporary}/localserver-logs"
printf '%s\n' 'private' >"${temporary}/localserver-logs/run.log"
git -C "${temporary}" add -Af

if "${temporary}/scripts/check-public-repository.sh" "${temporary}" >/dev/null 2>&1; then
  echo "policy check accepted forbidden localserver-logs directory" >&2
  exit 1
fi
