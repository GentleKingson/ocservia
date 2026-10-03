#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTROLLER="${ROOT}/deploy/bootstrap/install-controller"
NODE="${ROOT}/deploy/bootstrap/install-node"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT

# Published Stage-1 copies must be the actual executable sources.
mkdir "$fixture/prepared"
"$ROOT/scripts/prepare-bootstrap-release-assets.sh" "$fixture/prepared"
cmp "$fixture/prepared/controller-bootstrap.sh" "$ROOT/deploy/production/controller-bootstrap.sh"
cmp "$fixture/prepared/managed-node-bootstrap.sh" "$ROOT/deploy/managed-node/install.sh"
[[ -x "$fixture/prepared/controller-bootstrap.sh" && -x "$fixture/prepared/managed-node-bootstrap.sh" ]]

fail() {
  echo "stage-0 installer test: $1" >&2
  exit 1
}

mkdir -p "${fixture}/bin" "${fixture}/release" "${fixture}/tmp"
printf 'SHOULD_NOT_BE_READ=install-env-secret-must-not-leak\n' >"${fixture}/install.env"
cat >"${fixture}/bin/curl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
output=""
url="${!#}"
while (($# > 0)); do
  case "$1" in
    -o|--output) output="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf '%s\n' "${url}" >>"${TEST_DOWNLOAD_LOG}"
case "${TEST_DOWNLOAD_MODE:-success}" in
  tls) exit 35 ;;
  404-stage0)
    case "${url}" in */install-controller|*/install-node) exit 22 ;; esac
    ;;
  empty-stage0)
    case "${url}" in */install-controller|*/install-node) : >"${output}"; exit 0 ;; esac
    ;;
  truncated-stage0)
    case "${url}" in
      */install-controller|*/install-node)
        head -n -1 "${TEST_RELEASE_DIR}/${url##*/}" >"${output}"
        exit 0
        ;;
    esac
    ;;
  404-stage1)
    case "${url}" in */controller-bootstrap.sh|*/managed-node-bootstrap.sh) exit 22 ;; esac
    ;;
esac
cp -- "${TEST_RELEASE_DIR}/${url##*/}" "${output}"
MOCK
chmod 0700 "${fixture}/bin/curl"

cat >"${fixture}/release/controller-bootstrap.sh" <<'STAGE1'
#!/usr/bin/env bash
printf 'controller\n' >>"${TEST_EXEC_LOG}"
printf '%s\n' "$@" >"${TEST_ARGS_LOG}"
dirname -- "$0" >"${TEST_TEMP_LOG}"
STAGE1
cat >"${fixture}/release/managed-node-bootstrap.sh" <<'STAGE1'
#!/usr/bin/env bash
printf 'node\n' >>"${TEST_EXEC_LOG}"
printf '%s\n' "$@" >"${TEST_ARGS_LOG}"
dirname -- "$0" >"${TEST_TEMP_LOG}"
STAGE1
chmod 0700 "${fixture}/release/controller-bootstrap.sh" "${fixture}/release/managed-node-bootstrap.sh"

run_installer() {
  local installer="$1"; shift
  : >"${fixture}/downloads.log"
  : >"${fixture}/exec.log"
  : >"${fixture}/args.log"
  : >"${fixture}/temp.log"
  (
    cd "${fixture}"
    env \
      PATH="${fixture}/bin:${PATH}" \
      TMPDIR="${fixture}/tmp" \
      TEST_RELEASE_DIR="${fixture}/release" \
      TEST_DOWNLOAD_LOG="${fixture}/downloads.log" \
      TEST_EXEC_LOG="${fixture}/exec.log" \
      TEST_ARGS_LOG="${fixture}/args.log" \
      TEST_TEMP_LOG="${fixture}/temp.log" \
      BOOTSTRAP_TOKEN_SOURCE="token-must-not-leak" \
      OCSERV_PUBLIC_HOST="config-must-not-leak" \
      "${installer}" "$@"
  )
}

run_documented_fetch() (
  set -eu
  local endpoint="$1" mode="${2:-success}" stage0
  cd "${fixture}"
  stage0="$(TMPDIR="${fixture}/tmp" mktemp)" || exit 1
  trap 'rm -f -- "$stage0"' EXIT
  trap 'exit 1' HUP INT TERM
  PATH="${fixture}/bin:${PATH}" \
    TEST_RELEASE_DIR="${fixture}/release" \
    TEST_DOWNLOAD_LOG="${fixture}/downloads.log" \
    TEST_DOWNLOAD_MODE="${mode}" \
    curl -fsSL --proto '=https' --tlsv1.2 -o "${stage0}" \
      "https://get.ocservia.example/${endpoint}" || exit 1
  test "$(tail -n 1 "${stage0}")" = 'main "$@"' || exit 1
  PATH="${fixture}/bin:${PATH}" \
    TMPDIR="${fixture}/tmp" \
    TEST_RELEASE_DIR="${fixture}/release" \
    TEST_DOWNLOAD_LOG="${fixture}/downloads.log" \
    TEST_EXEC_LOG="${fixture}/exec.log" \
    TEST_ARGS_LOG="${fixture}/args.log" \
    TEST_TEMP_LOG="${fixture}/temp.log" \
    TEST_DOWNLOAD_MODE="${mode}" \
    bash "${stage0}" --version v1.2.3
)

for installer in "${CONTROLLER}" "${NODE}"; do
  for args in "" "--version latest" "--version v1.2.3-rc.0" "--version v1.2.3-rc.01" "--version v1.2.3-rc1" "--version v1.2.3-beta.1" "--version v1.2.3-rc.1+build" "--version main" "--version deadbeef"; do
    # shellcheck disable=SC2086 # each fixture intentionally supplies zero or two words
    if run_installer "${installer}" ${args} >"${fixture}/output" 2>&1; then
      fail "$(basename "${installer}") accepted a missing or non-release version: ${args:-<missing>}"
    fi
    [[ ! -s "${fixture}/exec.log" ]] || fail "invalid input reached Stage-1"
  done
done

for installer in "${CONTROLLER}" "${NODE}"; do
  run_installer "${installer}" --version v1.2.3-rc.1 >"${fixture}/output" 2>&1
  grep -qx 'v1.2.3-rc.1' "${fixture}/args.log" || fail "RC identity was not passed to Stage-1"
  grep -q '/download/v1.2.3-rc.1/' "${fixture}/downloads.log" || fail "RC download URL missing"
done

if run_installer "${NODE}" --version v1.2.3 --token token-must-not-leak >"${fixture}/output" 2>&1; then
  fail "Node Stage-0 accepted a non-allowlisted argument"
fi
[[ ! -s "${fixture}/exec.log" ]] || fail "a non-allowlisted argument reached Stage-1"

run_installer "${CONTROLLER}" --version v1.2.3 --root-lifecycle --check >"${fixture}/output" 2>&1
[[ "$(cat "${fixture}/exec.log")" == controller ]] || fail "Controller Stage-0 executed the wrong Stage-1"
[[ "$(<"${fixture}/args.log")" == $'--version\nv1.2.3\n--root-lifecycle\n--check' ]] ||
  fail "Controller Stage-0 did not pass the allowlisted arguments exactly"
grep -qx 'https://github.com/GentleKingson/ocservia/releases/download/v1.2.3/controller-bootstrap.sh' "${fixture}/downloads.log" ||
  fail "Controller Stage-0 did not construct the versioned release URL"

run_installer "${NODE}" --root-lifecycle --version v9.8.7 >"${fixture}/output" 2>&1
[[ "$(cat "${fixture}/exec.log")" == node ]] || fail "Node Stage-0 executed the wrong Stage-1"
[[ "$(<"${fixture}/args.log")" == $'--version\nv9.8.7\n--root-lifecycle' ]] ||
  fail "Node Stage-0 did not pass the allowlisted arguments exactly"
grep -qx 'https://github.com/GentleKingson/ocservia/releases/download/v9.8.7/managed-node-bootstrap.sh' "${fixture}/downloads.log" ||
  fail "Node Stage-0 did not construct the versioned release URL"

if grep -Eq 'token-must-not-leak|config-must-not-leak|install-env-secret-must-not-leak' "${fixture}/output"; then
  fail "Stage-0 leaked token or configuration content"
fi

for ((attempt = 0; attempt < 30; attempt++)); do
  temporary="$(<"${fixture}/temp.log")"
  [[ -n "${temporary}" && ! -e "${temporary}" ]] && break
  sleep 0.1
done
[[ -n "${temporary}" && ! -e "${temporary}" ]] || fail "successful handoff did not clean its temporary directory"

for asset in controller-bootstrap.sh managed-node-bootstrap.sh; do
  cp -- "${fixture}/release/${asset}" "${fixture}/stage1.good"
  : >"${fixture}/release/${asset}"
  installer="${CONTROLLER}"
  [[ "${asset}" != managed-node-bootstrap.sh ]] || installer="${NODE}"
  if run_installer "${installer}" --version v1.2.3 >"${fixture}/output" 2>&1; then
    fail "empty Stage-1 download unexpectedly succeeded"
  fi
  [[ ! -s "${fixture}/exec.log" ]] || fail "empty Stage-1 reached execution"
  mv -- "${fixture}/stage1.good" "${fixture}/release/${asset}"
done

for mode in 404-stage1 tls; do
  rm -rf -- "${fixture}/tmp"/*
  if TEST_DOWNLOAD_MODE="${mode}" run_installer "${CONTROLLER}" --version v1.2.3 >"${fixture}/output" 2>&1; then
    fail "${mode} download failure unexpectedly succeeded"
  fi
  [[ ! -s "${fixture}/exec.log" ]] || fail "${mode} download failure reached Stage-1"
  [[ -z "$(find "${fixture}/tmp" -mindepth 1 -print -quit)" ]] || fail "${mode} download failure leaked temporary files"
done

cp -- "${CONTROLLER}" "${fixture}/release/install-controller"
cp -- "${NODE}" "${fixture}/release/install-node"
cp -- "${ROOT}/install.env.example" "${fixture}/release/install.env.example"
for endpoint in install-controller install-node install.env.example; do
  case "${endpoint}" in
    install-controller) expected="${CONTROLLER}" ;;
    install-node) expected="${NODE}" ;;
    install.env.example) expected="${ROOT}/install.env.example" ;;
  esac
  PATH="${fixture}/bin:${PATH}" \
    TEST_RELEASE_DIR="${fixture}/release" \
    TEST_DOWNLOAD_LOG="${fixture}/downloads.log" \
    "${ROOT}/scripts/verify-bootstrap-endpoint.sh" \
    "https://get.ocservia.example/${endpoint}" "${expected}" >/dev/null
done
printf 'different bytes\n' >"${fixture}/different-source"
if PATH="${fixture}/bin:${PATH}" \
  TEST_RELEASE_DIR="${fixture}/release" \
  TEST_DOWNLOAD_LOG="${fixture}/downloads.log" \
  "${ROOT}/scripts/verify-bootstrap-endpoint.sh" \
  https://get.ocservia.example/install-controller "${fixture}/different-source" >/dev/null 2>&1; then
  fail "endpoint verifier accepted different deployed bytes"
fi

: >"${fixture}/exec.log"
for endpoint in install-controller install-node; do
  expected_stage1="${endpoint#install-}"
  run_documented_fetch "${endpoint}" success >"${fixture}/output" 2>&1 ||
    fail "the documented complete ${endpoint} fetch failed"
  [[ "$(tail -n 1 "${fixture}/exec.log")" == "${expected_stage1}" ]] ||
    fail "the documented complete ${endpoint} fetch did not reach its Stage-1"
done
for mode in tls 404-stage0 empty-stage0 truncated-stage0; do
  for endpoint in install-controller install-node; do
    : >"${fixture}/exec.log"
    if run_documented_fetch "${endpoint}" "${mode}" >"${fixture}/output" 2>&1; then
      fail "the documented ${endpoint} fetch accepted ${mode}"
    fi
    [[ ! -s "${fixture}/exec.log" ]] || fail "the documented ${mode} fetch reached Stage-1"
  done
done

head -n -1 "${CONTROLLER}" >"${fixture}/truncated"
chmod 0700 "${fixture}/truncated"
: >"${fixture}/exec.log"
set +e
PATH="${fixture}/bin:${PATH}" TEST_EXEC_LOG="${fixture}/exec.log" "${fixture}/truncated" --version v1.2.3
set -e
[[ ! -s "${fixture}/exec.log" ]] || fail "a truncated Stage-0 script executed main"

for installer in "${CONTROLLER}" "${NODE}"; do
  [[ "$(tail -n 1 "${installer}")" == 'main "$@"' ]] || fail "main invocation is not the final line"
  if grep -Eq '(^|[^[:alnum:]_])(sudo|apt|apt-get|dnf|yum|rpm|dpkg|docker|systemctl|psql)([^[:alnum:]_]|$)' "${installer}"; then
    fail "$(basename "${installer}") contains forbidden production authority"
  fi
done

echo "Stage-0 installer tests passed"
