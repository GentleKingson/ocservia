#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTROLLER="${ROOT}/deploy/bootstrap/install-controller"
NODE="${ROOT}/deploy/bootstrap/install-node"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT

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
    --output) output="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf '%s\n' "${url}" >>"${TEST_DOWNLOAD_LOG}"
case "${TEST_DOWNLOAD_MODE:-success}" in
  tls) exit 35 ;;
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

openssl genpkey -algorithm ED25519 -out "${fixture}/release-key.pem" >/dev/null 2>&1
openssl pkey -in "${fixture}/release-key.pem" -pubout -out "${fixture}/release-key.pub.pem" >/dev/null 2>&1
(
  cd "${fixture}/release"
  sha256sum controller-bootstrap.sh managed-node-bootstrap.sh >SHA256SUMS
  openssl pkeyutl -sign -rawin -inkey "${fixture}/release-key.pem" -in SHA256SUMS -out SHA256SUMS.sig
)
openssl pkey -pubin -in "${fixture}/release-key.pub.pem" -outform DER -out "${fixture}/release-key.der" >/dev/null 2>&1
fingerprint="$(sha256sum "${fixture}/release-key.der")"
fingerprint="${fingerprint%% *}"

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
      TRUSTED_RELEASE_KEY="${fixture}/release-key.pub.pem" \
      EXPECTED_RELEASE_KEY_SHA256="${fingerprint}" \
      BOOTSTRAP_TOKEN_SOURCE="token-must-not-leak" \
      OCSERV_PUBLIC_HOST="config-must-not-leak" \
      "${installer}" "$@"
  )
}

for installer in "${CONTROLLER}" "${NODE}"; do
  for args in "" "--version latest" "--version v1.2.3-rc.1" "--version main" "--version deadbeef"; do
    # shellcheck disable=SC2086 # each fixture intentionally supplies zero or two words
    if run_installer "${installer}" ${args} >"${fixture}/output" 2>&1; then
      fail "$(basename "${installer}") accepted a missing or non-release version: ${args:-<missing>}"
    fi
    [[ ! -s "${fixture}/exec.log" ]] || fail "invalid input reached Stage-1"
  done
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
  fail "Controller Stage-0 did not construct the immutable release URL"

run_installer "${NODE}" --root-lifecycle --version v9.8.7 >"${fixture}/output" 2>&1
[[ "$(cat "${fixture}/exec.log")" == node ]] || fail "Node Stage-0 executed the wrong Stage-1"
[[ "$(<"${fixture}/args.log")" == $'--version\nv9.8.7\n--root-lifecycle' ]] ||
  fail "Node Stage-0 did not pass the allowlisted arguments exactly"
grep -qx 'https://github.com/GentleKingson/ocservia/releases/download/v9.8.7/managed-node-bootstrap.sh' "${fixture}/downloads.log" ||
  fail "Node Stage-0 did not construct the immutable release URL"

if grep -Eq 'token-must-not-leak|config-must-not-leak|install-env-secret-must-not-leak' "${fixture}/output"; then
  fail "Stage-0 leaked token or configuration content"
fi

for ((attempt = 0; attempt < 30; attempt++)); do
  temporary="$(<"${fixture}/temp.log")"
  [[ -n "${temporary}" && ! -e "${temporary}" ]] && break
  sleep 0.1
done
[[ -n "${temporary}" && ! -e "${temporary}" ]] || fail "successful handoff did not clean its temporary directory"

cp -- "${fixture}/release/managed-node-bootstrap.sh" "${fixture}/managed-node-bootstrap.good"
printf '\n# tampered\n' >>"${fixture}/release/managed-node-bootstrap.sh"
if run_installer "${NODE}" --version v1.2.3 >"${fixture}/output" 2>&1; then
  fail "Node Stage-0 executed a Stage-1 asset whose digest did not match"
fi
[[ ! -s "${fixture}/exec.log" ]] || fail "a digest mismatch reached Stage-1"
mv -- "${fixture}/managed-node-bootstrap.good" "${fixture}/release/managed-node-bootstrap.sh"

for mode in 404-stage1 tls; do
  rm -rf -- "${fixture}/tmp"/*
  if TEST_DOWNLOAD_MODE="${mode}" run_installer "${CONTROLLER}" --version v1.2.3 >"${fixture}/output" 2>&1; then
    fail "${mode} download failure unexpectedly succeeded"
  fi
  [[ ! -s "${fixture}/exec.log" ]] || fail "${mode} download failure reached Stage-1"
  [[ -z "$(find "${fixture}/tmp" -mindepth 1 -print -quit)" ]] || fail "${mode} download failure leaked temporary files"
done

cp -- "${CONTROLLER}" "${fixture}/release/install-controller"
PATH="${fixture}/bin:${PATH}" \
  TEST_RELEASE_DIR="${fixture}/release" \
  TEST_DOWNLOAD_LOG="${fixture}/downloads.log" \
  "${ROOT}/scripts/verify-bootstrap-endpoint.sh" \
  https://get.ocservia.example/install-controller "${CONTROLLER}" >/dev/null
printf 'different bytes\n' >"${fixture}/different-source"
if PATH="${fixture}/bin:${PATH}" \
  TEST_RELEASE_DIR="${fixture}/release" \
  TEST_DOWNLOAD_LOG="${fixture}/downloads.log" \
  "${ROOT}/scripts/verify-bootstrap-endpoint.sh" \
  https://get.ocservia.example/install-controller "${fixture}/different-source" >/dev/null 2>&1; then
  fail "endpoint verifier accepted different deployed bytes"
fi

head -n -1 "${CONTROLLER}" >"${fixture}/truncated"
chmod 0700 "${fixture}/truncated"
: >"${fixture}/exec.log"
PATH="${fixture}/bin:${PATH}" TEST_EXEC_LOG="${fixture}/exec.log" "${fixture}/truncated" --version v1.2.3
[[ ! -s "${fixture}/exec.log" ]] || fail "a truncated Stage-0 script executed main"

for installer in "${CONTROLLER}" "${NODE}"; do
  [[ "$(tail -n 1 "${installer}")" == 'main "$@"' ]] || fail "main invocation is not the final line"
  if grep -Eq '(^|[^[:alnum:]_])(sudo|apt|apt-get|dnf|yum|rpm|dpkg|docker|systemctl|psql)([^[:alnum:]_]|$)' "${installer}"; then
    fail "$(basename "${installer}") contains forbidden production authority"
  fi
done

echo "Stage-0 installer tests passed"
