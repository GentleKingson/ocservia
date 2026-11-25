#!/usr/bin/env bash
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${ROOT}/scripts/env.sh"
cd "${ROOT}"
: "${CI_SUITES:?selected tool contracts are required}"

for suite in ${CI_SUITES}; do
  case "${suite}" in
    guards|release|source-style|contracts) ;;
    *) echo "unknown CI tool suite: ${suite}" >&2; exit 2 ;;
  esac
done

work="$(mktemp -d)"
trap 'rm -rf -- "${work}"' EXIT

run_guards() {
  local script
  for script in scripts/ci-*.sh scripts/required-go-tests.sh scripts/bootstrap.sh; do
    bash -n "${script}"
  done
  bash scripts/test-ci-relevance.sh
  bash scripts/test-toolchain-consistency.sh
  python3 scripts/test-workflow-contracts.py
  bash scripts/check-public-repository.sh
}

run_release() {
  local idx status
  local -a pids=() logs=() cmds=()
  bash -n scripts/release-common.sh scripts/release-business-probe.sh scripts/release-quick-acceptance.sh
  cmds=(
    "node scripts/test-release-upgrade.mjs"
    "bash scripts/test-controller-release-manifest.sh"
    "python3 scripts/test-release-signer-window.py"
    "bash scripts/test-release-image-security.sh"
    "bash scripts/test-release-package-security.sh"
    "bash scripts/test-controller-quick-materials.sh"
  )
  for idx in "${!cmds[@]}"; do
    logs[idx]="${work}/release-${idx}.log"
    bash -euo pipefail -c "${cmds[idx]}" >"${logs[idx]}" 2>&1 &
    pids[idx]=$!
  done
  status=0
  for idx in "${!pids[@]}"; do
    if ! wait "${pids[idx]}"; then
      status=1
    fi
    cat "${logs[idx]}"
  done
  return "${status}"
}

run_source_style() {
  bash scripts/lint.sh source-style
}

has_contracts=false
base_sha=""
for suite in ${CI_SUITES}; do
  if [[ "${suite}" == contracts ]]; then
    has_contracts=true
    : "${CONTRACT_BASE_REF:?contract_base_ref is required for contract checks}"
    baseline="${CONTRACT_BASE_REF}"
    if ! git rev-parse --verify --end-of-options "${baseline}^{commit}" >/dev/null 2>&1; then
      git fetch --no-tags -- origin "${baseline}"
      baseline="$(git rev-parse --verify 'FETCH_HEAD^{commit}')"
    fi
    if [[ "${CONTRACT_EVENT:-}" == pull_request ]]; then
      baseline="$(git merge-base "${baseline}" "${CONTRACT_HEAD_SHA:?PR head is required}")"
    fi
    base_sha="$(git rev-parse --verify --end-of-options "${baseline}^{commit}")"
  fi
done

pids=()
logs=()
idx=0
for suite in ${CI_SUITES}; do
  case "${suite}" in
    guards)
      logs[idx]="${work}/suite-guards.log"
      run_guards >"${logs[idx]}" 2>&1 &
      pids[idx]=$!
      idx=$((idx + 1))
      ;;
    release)
      logs[idx]="${work}/suite-release.log"
      run_release >"${logs[idx]}" 2>&1 &
      pids[idx]=$!
      idx=$((idx + 1))
      ;;
    source-style)
      logs[idx]="${work}/suite-source-style.log"
      run_source_style >"${logs[idx]}" 2>&1 &
      pids[idx]=$!
      idx=$((idx + 1))
      ;;
    contracts)
      logs[idx]="${work}/suite-contracts-lint.log"
      bash scripts/lint.sh contracts >"${logs[idx]}" 2>&1 &
      pids[idx]=$!
      idx=$((idx + 1))

      logs[idx]="${work}/suite-contracts-breaking.log"
      bash scripts/check-breaking.sh "${base_sha}" >"${logs[idx]}" 2>&1 &
      pids[idx]=$!
      idx=$((idx + 1))
      ;;
  esac
done

overall=0
for i in "${!pids[@]}"; do
  if ! wait "${pids[i]}"; then
    overall=1
  fi
  cat "${logs[i]}"
done
if ((overall != 0)); then
  exit 1
fi

if [[ "${has_contracts}" == true ]]; then
  bash scripts/generated-clean.sh
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    printf 'contract_base_sha=%s\n' "${base_sha}" >>"${GITHUB_OUTPUT}"
  fi
fi
