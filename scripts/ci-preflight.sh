#!/usr/bin/env bash
# Reproduce PR path selection and the ci-tools job for the checked-out commit.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
if (($# > 1)); then echo 'usage: ci-preflight.sh [base-ref]' >&2; exit 2; fi
base=$(git rev-parse --verify --end-of-options "${1:-origin/main}^{commit}")
head=$(git rev-parse HEAD)
changes=$(git status --porcelain)
if [[ -n "${changes}" ]]; then
  printf 'Changes outside candidate %s:\n%s\nRun from a clean candidate checkout.\n' "${head}" "${changes}" >&2
  exit 1
fi
routing=$(mktemp)
trap 'rm -f -- "${routing}"' EXIT
bash scripts/ci-relevance.sh pull_request "${base}" "${head}" "${routing}"
cat "${routing}"
suites=$(sed -n 's/^ci_suites=//p' "${routing}")
if [[ -n "${suites}" ]]; then
  CI_SUITES="${suites}" CONTRACT_EVENT=pull_request CONTRACT_BASE_REF="${base}" CONTRACT_HEAD_SHA="${head}" bash scripts/ci-tools-check.sh
fi
