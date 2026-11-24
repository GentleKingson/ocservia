#!/usr/bin/env bash
# Regression checks for Basic CI path routing (scripts/ci-relevance.sh).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

flags=()
reason=''
ci_suites=()
ci_tools_profiles=''
ci_tools_cache_label=''
controller_cache_images=()

# Load the classifier tables and helpers without running the git-diff entrypoint.
eval "$(awk '/^flags=/{keep=1} /^valid_sha\(\)/{keep=0} keep' scripts/ci-relevance.sh)"

reset_state() {
  local flag
  for flag in "${flags[@]}"; do printf -v "${flag}" false; done
  reason=recognized_paths
  ci_suites=()
  ci_tools_profiles=''
  ci_tools_cache_label=''
  controller_cache_images=()
}

die() {
  echo "test-ci-relevance: $*" >&2
  exit 1
}

assert_eq() {
  local label="$1" expected="$2" actual="$3"
  [[ "${actual}" == "${expected}" ]] || die "${label}: expected '${expected}', got '${actual}'"
}

assert_flag() {
  local label="$1" flag="$2" expected="$3"
  assert_eq "${label} ${flag}" "${expected}" "${!flag}"
}

# 1. Every tracked file must match a declared rule (never fall through to unknown_path).
while IFS= read -r path; do
  reset_state
  classify_path "${path}"
  [[ "${reason}" != unknown_path:* ]] || die "tracked file fell through to ${reason}"
done < <(git ls-files)

# 2. Unknown paths, shared contracts, toolchains and shared CI actions must fail closed.
for path in unknown-top/probe.txt proto/buf.yaml openapi/openapi.yaml toolchains.lock .github/actions/setup-tools/action.yml; do
  reset_state
  classify_path "${path}"
  [[ "${reason}" != recognized_paths ]] || die "${path} did not fail closed"
  for flag in "${flags[@]}"; do
    assert_flag "${path}" "${flag}" true
  done
  assert_eq "${path} ci_suites" "guards release source-style contracts" "${ci_suites[*]}"
  assert_eq "${path} controller_cache_images" "gateway control transport backup edge relay signer" "${controller_cache_images[*]}"
done

# 3. Targeted routing checks for key subsystems and boundaries.
reset_state
classify_path docs/architecture.md
assert_flag docs run_docs true
assert_flag docs run_go false
assert_flag docs run_ci_tools false

reset_state
classify_path web/test/router.test.ts
assert_flag "web unit test" run_web true
assert_flag "web unit test" run_web_browser false
assert_flag "web unit test" run_controller_cache false

reset_state
classify_path web/src/features/nodes/node-list.ts
assert_flag "web ui" run_web true
assert_flag "web ui" run_web_browser true
assert_eq "web ui cache" gateway "${controller_cache_images[*]}"

reset_state
classify_path deploy/production/integrated/nginx.conf.template
assert_eq "integrated nginx cache" edge "${controller_cache_images[*]}"
assert_flag "integrated nginx" run_installers true

reset_state
classify_path scripts/transport-boundary-check.sh
assert_flag "transport boundary" run_go true
assert_flag "transport boundary" run_rust true

reset_state
classify_path control-plane/internal/foo.go
assert_flag "go internal" run_go true
assert_flag "go internal" run_database true
assert_eq "go internal cache" control "${controller_cache_images[*]}"

reset_state
classify_path control-plane/internal/foo_test.go
assert_flag "go test" run_go true
assert_flag "go test" run_database true
assert_flag "go test" run_controller_cache false
assert_eq "go test ci_suites" guards "${ci_suites[*]}"

reset_state
classify_path .github/workflows/ci.yml
resolve_ci_tools_profiles
assert_flag ci.yml run_ci_tools true
assert_eq "ci.yml ci_suites" guards "${ci_suites[*]}"
assert_eq "ci.yml ci_tools_profiles" "" "${ci_tools_profiles}"
assert_flag ci.yml run_go false

reset_state
classify_path .github/workflows/release.yml
resolve_ci_tools_profiles
assert_eq "release.yml ci_suites" "guards release" "${ci_suites[*]}"
assert_eq "release.yml ci_tools_profiles" "node" "${ci_tools_profiles}"
assert_eq "release.yml ci_tools_cache_label" "node" "${ci_tools_cache_label}"
assert_flag "release.yml" run_installers false

reset_state
classify_path scripts/release_business_scenarios.py
resolve_ci_tools_profiles
assert_eq "release business scenarios ci_suites" "release source-style" "${ci_suites[*]}"
assert_eq "release business scenarios profiles" "source-style node" "${ci_tools_profiles}"
assert_eq "release business scenarios label" "source-style-node" "${ci_tools_cache_label}"
assert_flag "release business scenarios" run_installers true

reset_state
classify_path deploy/systemd/ocservia-privd.service
assert_flag "systemd unit" run_rust true
assert_flag "systemd unit" run_installers true
assert_eq "systemd unit ci_suites" release "${ci_suites[*]}"

reset_state
classify_path deploy/lib/quick-materials.sh
assert_flag "quick materials" run_installers true
assert_eq "quick materials ci_suites" "release source-style" "${ci_suites[*]}"

reset_state
classify_path web/e2e/node-install.spec.ts
assert_flag "web e2e spec" run_web true
assert_flag "web e2e spec" run_web_browser true
assert_eq "web e2e spec ci_suites" guards "${ci_suites[*]}"

reset_state
classify_path scripts/required-go-tests.txt
assert_flag "required go tests" run_database true
assert_eq "required go tests ci_suites" guards "${ci_suites[*]}"

reset_state
classify_path scripts/go-test-environment.sh
assert_flag "go test env" run_go true
assert_flag "go test env" run_database true
assert_eq "go test env ci_suites" "guards source-style" "${ci_suites[*]}"

# Every shell/Python source scanned by scripts/lint.sh source-style must trigger source-style.
while IFS= read -r -d '' path; do
  [[ -f "${path}" ]] || continue
  IFS= read -r shebang <"${path}" || true
  if [[ "${path}" == *.py ]] || [[ "${shebang}" =~ ^\#!.*(/|[[:space:]])(ba)?sh([[:space:]]|$) ]]; then
    reset_state
    classify_path "${path}"
    case " ${ci_suites[*]:-} " in
      *" source-style "*) ;;
      *) die "linted source file ${path} did not select source-style (got '${ci_suites[*]:-}')" ;;
    esac
  fi
done < <(git ls-files -z -- scripts deploy)

# 4. End-to-end CLI checks (workflow_dispatch and fail-closed git error handling).
out_file="$(mktemp)"
trap 'rm -f -- "${out_file}"' EXIT

read_out() {
  awk -F= -v k="$1" '$1 == k { print substr($0, length(k) + 2) }' "${out_file}"
}

head_sha="$(git rev-parse HEAD)"

: >"${out_file}"
bash scripts/ci-relevance.sh workflow_dispatch "${head_sha}" "${head_sha}" "${out_file}" full
assert_eq "dispatch full profile" full "$(read_out profile)"
assert_eq "dispatch full database_scope" full "$(read_out database_scope)"
assert_eq "dispatch full ci_suites" "guards release source-style contracts" "$(read_out ci_suites)"
assert_eq "dispatch full ci_tools_profiles" "source-style contracts" "$(read_out ci_tools_profiles)"
assert_eq "dispatch full ci_tools_cache_label" "source-style-contracts" "$(read_out ci_tools_cache_label)"
assert_eq "dispatch full run_installers" true "$(read_out run_installers)"
assert_eq "dispatch full run_controller_cache" false "$(read_out run_controller_cache)"

: >"${out_file}"
bash scripts/ci-relevance.sh workflow_dispatch "${head_sha}" "${head_sha}" "${out_file}" quick
assert_eq "dispatch quick profile" quick "$(read_out profile)"
assert_eq "dispatch quick database_scope" smoke "$(read_out database_scope)"

: >"${out_file}"
bash scripts/ci-relevance.sh push 0000000000000000000000000000000000000000 "${head_sha}" "${out_file}" quick
assert_eq "zero before sha reason" all_zero_before_sha "$(read_out reason)"
assert_eq "zero before sha run_go" true "$(read_out run_go)"

: >"${out_file}"
bash scripts/ci-relevance.sh pull_request not-a-sha "${head_sha}" "${out_file}" quick
assert_eq "invalid sha reason" invalid_sha "$(read_out reason)"
assert_eq "invalid sha run_rust" true "$(read_out run_rust)"

: >"${out_file}"
bash scripts/ci-relevance.sh schedule "${head_sha}" "${head_sha}" "${out_file}" quick
assert_eq "unsupported event reason" unsupported_event:schedule "$(read_out reason)"
assert_eq "unsupported event run_web" true "$(read_out run_web)"
