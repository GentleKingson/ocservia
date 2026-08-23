#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${ROOT}/scripts/g6-smoke-relevance.sh"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT

git -C "${fixture}" init -q
git -C "${fixture}" config user.name test
git -C "${fixture}" config user.email test@example.invalid
mkdir -p "${fixture}/docs/acceptance" "${fixture}/control-plane"
printf 'before\n' >"${fixture}/docs/guide.md"
printf 'before\n' >"${fixture}/docs/acceptance/g6-slo.yaml"
printf 'before\n' >"${fixture}/control-plane/main.go"
git -C "${fixture}" add .
git -C "${fixture}" commit -qm base
base="$(git -C "${fixture}" rev-parse HEAD)"

printf 'after\n' >"${fixture}/docs/guide.md"
git -C "${fixture}" commit -qam docs
docs_head="$(git -C "${fixture}" rev-parse HEAD)"
docs_output="${fixture}/docs.output"
(trap - EXIT; cd "${fixture}" && "${SCRIPT}" "${base}" "${docs_head}" "${docs_output}")
grep -qx 'relevant=false' "${docs_output}"
grep -qx 'reason=documentation_only' "${docs_output}"

printf 'after\n' >"${fixture}/docs/acceptance/g6-slo.yaml"
git -C "${fixture}" commit -qam contract
contract_head="$(git -C "${fixture}" rev-parse HEAD)"
contract_output="${fixture}/contract.output"
(trap - EXIT; cd "${fixture}" && "${SCRIPT}" "${docs_head}" "${contract_head}" "${contract_output}")
grep -qx 'relevant=true' "${contract_output}"
grep -qx 'reason=executable_or_contract_change' "${contract_output}"

printf 'after\n' >"${fixture}/control-plane/main.go"
git -C "${fixture}" commit -qam code
code_head="$(git -C "${fixture}" rev-parse HEAD)"
code_output="${fixture}/code.output"
(trap - EXIT; cd "${fixture}" && "${SCRIPT}" "${contract_head}" "${code_head}" "${code_output}")
grep -qx 'relevant=true' "${code_output}"
grep -qx 'reason=executable_or_contract_change' "${code_output}"

empty_output="${fixture}/empty.output"
(trap - EXIT; cd "${fixture}" && "${SCRIPT}" "${code_head}" "${code_head}" "${empty_output}")
grep -qx 'relevant=true' "${empty_output}"
grep -qx 'reason=empty_diff_fail_closed' "${empty_output}"
