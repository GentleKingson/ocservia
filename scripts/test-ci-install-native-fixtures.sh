#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALLER="${ROOT}/scripts/ci-install-native-fixtures.sh"
TEMP_BASE="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
TEMP_ROOT="$(mktemp -d "${TEMP_BASE%/}/ocservia-native-fixtures-test.XXXXXX")"
trap 'rm -rf -- "${TEMP_ROOT}"' EXIT

fail() {
  echo "$*" >&2
  exit 1
}

mkdir -p "${TEMP_ROOT}/bin"
cat >"${TEMP_ROOT}/bin/timeout" <<'SH'
#!/usr/bin/env bash
set -euo pipefail

[[ "$#" -ge 4 ]] || exit 90
[[ "$1" == "--signal=TERM" ]] || exit 91
[[ "$2" == "--kill-after=10s" ]] || exit 92
duration="$3"
shift 3
printf 'timeout duration=%s command=%s\n' "${duration}" "$1" >>"${FIXTURE_LOG}"
"$@"
SH
chmod +x "${TEMP_ROOT}/bin/timeout"

cat >"${TEMP_ROOT}/bin/apt-get" <<'SH'
#!/usr/bin/env bash
set -euo pipefail

action=""
install_tail=()
after_install=0
for argument in "$@"; do
  if [[ "${after_install}" -eq 1 ]]; then
    install_tail+=("${argument}")
  fi
  case "${argument}" in
    update | install)
      action="${argument}"
      if [[ "${argument}" == "install" ]]; then
        after_install=1
      fi
      ;;
  esac
done
[[ -n "${action}" ]] || exit 93

{
  printf 'apt-get action=%s' "${action}"
  printf ' %q' "$@"
  printf '\n'
} >>"${FIXTURE_LOG}"

canonical_sources() {
  ! grep -R -Fq 'azure.archive.ubuntu.com' \
    "${FIXTURE_APT_ROOT}/sources.list" \
    "${FIXTURE_APT_ROOT}/sources.list.d" \
    "${FIXTURE_APT_ROOT}/apt-mirrors.txt" \
    || return 1
  grep -R -Fq 'https://archive.ubuntu.com/ubuntu' \
    "${FIXTURE_APT_ROOT}/sources.list" \
    "${FIXTURE_APT_ROOT}/sources.list.d" \
    "${FIXTURE_APT_ROOT}/apt-mirrors.txt"
}

if [[ "${action}" == "update" ]]; then
  count=0
  if [[ -s "${FIXTURE_UPDATE_COUNT}" ]]; then
    read -r count <"${FIXTURE_UPDATE_COUNT}"
  fi
  count=$((count + 1))
  printf '%s\n' "${count}" >"${FIXTURE_UPDATE_COUNT}"
  IFS=',' read -r -a statuses <<<"${FIXTURE_UPDATE_STATUSES}"
  [[ "${count}" -le "${#statuses[@]}" ]] || exit 94
  if [[ "${count}" -eq 2 ]]; then
    canonical_sources || exit 95
    echo 'source-state attempt=2 mirror=archive.ubuntu.com' >>"${FIXTURE_LOG}"
  fi
  exit "${statuses[$((count - 1))]}"
fi

count=0
if [[ -s "${FIXTURE_INSTALL_COUNT}" ]]; then
  read -r count <"${FIXTURE_INSTALL_COUNT}"
fi
count=$((count + 1))
printf '%s\n' "${count}" >"${FIXTURE_INSTALL_COUNT}"
expected_install_tail=(--yes ocserv openconnect ssl-cert)
[[ "${#install_tail[@]}" -eq "${#expected_install_tail[@]}" ]] || exit 97
for index in "${!expected_install_tail[@]}"; do
  [[ "${install_tail[$index]}" == "${expected_install_tail[$index]}" ]] || exit 97
done
if [[ "${FIXTURE_EXPECT_CANONICAL}" -eq 1 ]]; then
  canonical_sources || exit 96
fi
if [[ "${FIXTURE_INSTALL_STATUS}" -ne 0 ]]; then
  exit "${FIXTURE_INSTALL_STATUS}"
fi

for installed_command in ocserv ocpasswd openconnect; do
  if [[ "${installed_command}" == "${FIXTURE_MISSING_COMMAND}" ]]; then
    continue
  fi
  printf '#!/usr/bin/env bash\nexit 0\n' >"${FIXTURE_COMMAND_DIR}/${installed_command}"
  chmod +x "${FIXTURE_COMMAND_DIR}/${installed_command}"
done
SH
chmod +x "${TEMP_ROOT}/bin/apt-get"

RUN_STATUS=0
RUN_OUTPUT=""
RUN_LOG=""
RUN_UPDATE_COUNT=0
RUN_INSTALL_COUNT=0

run_case() {
  local name="$1"
  local update_statuses="$2"
  local install_status="$3"
  local expect_canonical="$4"
  local missing_command="$5"
  local case_root="${TEMP_ROOT}/${name}"
  local apt_root="${case_root}/apt"
  local command_dir="${case_root}/commands"
  local runner_temp="${case_root}/runner-temp"
  local status

  mkdir -p "${apt_root}/sources.list.d" "${command_dir}" "${runner_temp}"
  cat >"${apt_root}/sources.list" <<'EOF'
deb http://azure.archive.ubuntu.com/ubuntu noble main universe
EOF
  cat >"${apt_root}/apt-mirrors.txt" <<'EOF'
http://azure.archive.ubuntu.com/ubuntu
https://archive.ubuntu.com/ubuntu
EOF
  cat >"${apt_root}/sources.list.d/ubuntu.sources" <<EOF
Types: deb
URIs: mirror+file:${apt_root}/apt-mirrors.txt
Suites: noble noble-updates noble-backports noble-security
Components: main universe
EOF
  cat >"${apt_root}/sources.list.d/vendor.list" <<'EOF'
deb https://packages.example.invalid/ubuntu noble main
EOF
  cp -a -- "${apt_root}" "${case_root}/baseline"
  : >"${case_root}/fixture.log"
  : >"${case_root}/update-count"
  : >"${case_root}/install-count"

  set +e
  RUN_OUTPUT="$(
    PATH="${TEMP_ROOT}/bin:${command_dir}:${PATH}" \
      RUNNER_TEMP="${runner_temp}" \
      OCSERVIA_CI_APT_ETC_DIR="${apt_root}" \
      OCSERVIA_CI_APT_GET_BIN="${TEMP_ROOT}/bin/apt-get" \
      OCSERVIA_CI_TIMEOUT_BIN="${TEMP_ROOT}/bin/timeout" \
      FIXTURE_APT_ROOT="${apt_root}" \
      FIXTURE_COMMAND_DIR="${command_dir}" \
      FIXTURE_EXPECT_CANONICAL="${expect_canonical}" \
      FIXTURE_INSTALL_COUNT="${case_root}/install-count" \
      FIXTURE_INSTALL_STATUS="${install_status}" \
      FIXTURE_LOG="${case_root}/fixture.log" \
      FIXTURE_MISSING_COMMAND="${missing_command}" \
      FIXTURE_UPDATE_COUNT="${case_root}/update-count" \
      FIXTURE_UPDATE_STATUSES="${update_statuses}" \
      "${INSTALLER}" 2>&1
  )"
  status=$?
  set -e

  RUN_STATUS="${status}"
  RUN_LOG="$(<"${case_root}/fixture.log")"
  RUN_UPDATE_COUNT=0
  RUN_INSTALL_COUNT=0
  if [[ -s "${case_root}/update-count" ]]; then
    read -r RUN_UPDATE_COUNT <"${case_root}/update-count"
  fi
  if [[ -s "${case_root}/install-count" ]]; then
    read -r RUN_INSTALL_COUNT <"${case_root}/install-count"
  fi
  diff -ru -- "${case_root}/baseline" "${apt_root}" \
    || fail "${name}: installer did not restore the original APT source state"
}

assert_common_options() {
  local name="$1"
  local apt_line

  while IFS= read -r apt_line; do
    [[ "${apt_line}" == *"Acquire::Retries=2"* ]] \
      || fail "${name}: bounded retry option is missing"
    [[ "${apt_line}" == *"Acquire::http::Timeout=10"* ]] \
      || fail "${name}: HTTP request timeout is missing"
    [[ "${apt_line}" == *"Acquire::https::Timeout=10"* ]] \
      || fail "${name}: HTTPS request timeout is missing"
  done < <(printf '%s\n' "${RUN_LOG}" | grep '^apt-get action=')
}

assert_update_error_mode() {
  local name="$1"
  local update_line

  while IFS= read -r update_line; do
    [[ "${update_line}" == *"APT::Update::Error-Mode=any"* ]] \
      || fail "${name}: partial update failures are not fail-closed"
  done < <(printf '%s\n' "${RUN_LOG}" | grep '^apt-get action=update')
}

assert_diagnostic() {
  local name="$1"
  local pattern="$2"

  printf '%s\n' "${RUN_OUTPUT}" | grep -Eq "${pattern}" \
    || fail "${name}: diagnostic line is missing or malformed"
}

run_case primary-success '0' 0 0 ''
[[ "${RUN_STATUS}" -eq 0 ]] || fail "primary-success: expected success"
[[ "${RUN_UPDATE_COUNT}" -eq 1 ]] || fail "primary-success: fallback was unexpectedly used"
[[ "${RUN_INSTALL_COUNT}" -eq 1 ]] || fail "primary-success: packages were not installed exactly once"
assert_diagnostic primary-success \
  '^native-fixtures: apt-update attempt=1 mirror=azure[.]archive[.]ubuntu[.]com exit_code=0 elapsed_seconds=[0-9]+$'
[[ "${RUN_OUTPUT}" != *'apt-update attempt=2'* ]] || fail "primary-success: fallback diagnostics were emitted"
[[ "$(printf '%s\n' "${RUN_LOG}" | grep -c '^timeout duration=150s command=')" -eq 1 ]] \
  || fail "primary-success: update does not have one 150-second outer timeout"
[[ "$(printf '%s\n' "${RUN_LOG}" | grep -c '^timeout duration=300s command=')" -eq 1 ]] \
  || fail "primary-success: install does not have one outer timeout"
assert_common_options primary-success
assert_update_error_mode primary-success
for installed_command in ocserv ocpasswd openconnect; do
  [[ "${RUN_OUTPUT}" == *"native-fixtures: verified command=${installed_command}"* ]] \
    || fail "primary-success: ${installed_command} was not verified"
done

run_case fallback-success '124,0' 0 1 ''
[[ "${RUN_STATUS}" -eq 0 ]] || fail "fallback-success: expected success"
[[ "${RUN_UPDATE_COUNT}" -eq 2 ]] || fail "fallback-success: expected one primary and one fallback update"
[[ "${RUN_INSTALL_COUNT}" -eq 1 ]] || fail "fallback-success: packages were not installed"
assert_diagnostic fallback-success \
  '^native-fixtures: apt-update attempt=1 mirror=azure[.]archive[.]ubuntu[.]com exit_code=124 elapsed_seconds=[0-9]+$'
assert_diagnostic fallback-success \
  '^native-fixtures: apt-update attempt=2 mirror=archive[.]ubuntu[.]com exit_code=0 elapsed_seconds=[0-9]+$'
[[ "${RUN_LOG}" == *'source-state attempt=2 mirror=archive.ubuntu.com'* ]] \
  || fail "fallback-success: fallback did not use the official archive"
[[ "$(printf '%s\n' "${RUN_LOG}" | grep -c '^timeout duration=150s command=')" -eq 2 ]] \
  || fail "fallback-success: update timeout was not applied exactly twice"
assert_common_options fallback-success
assert_update_error_mode fallback-success

run_case both-updates-fail '124,100' 0 1 ''
[[ "${RUN_STATUS}" -ne 0 ]] || fail "both-updates-fail: expected fail-closed status"
[[ "${RUN_UPDATE_COUNT}" -eq 2 ]] || fail "both-updates-fail: fallback loop was not bounded"
[[ "${RUN_INSTALL_COUNT}" -eq 0 ]] || fail "both-updates-fail: install ran after update failure"
assert_diagnostic both-updates-fail \
  '^native-fixtures: apt-update attempt=2 mirror=archive[.]ubuntu[.]com exit_code=100 elapsed_seconds=[0-9]+$'
[[ "${RUN_OUTPUT}" == *'fallback update failed closed'* ]] \
  || fail "both-updates-fail: fail-closed reason is missing"
assert_common_options both-updates-fail
assert_update_error_mode both-updates-fail

run_case install-fails '0' 55 0 ''
[[ "${RUN_STATUS}" -ne 0 ]] || fail "install-fails: expected fail-closed status"
[[ "${RUN_INSTALL_COUNT}" -eq 1 ]] || fail "install-fails: install attempt count changed"
assert_diagnostic install-fails \
  '^native-fixtures: apt-install attempt=1 mirror=azure[.]archive[.]ubuntu[.]com exit_code=55 elapsed_seconds=[0-9]+$'

for missing_command in ocserv ocpasswd openconnect; do
  run_case "missing-${missing_command}" '0' 0 0 "${missing_command}"
  [[ "${RUN_STATUS}" -ne 0 ]] \
    || fail "missing-${missing_command}: expected command verification failure"
  [[ "${RUN_OUTPUT}" == *"installed command is unavailable: ${missing_command}"* ]] \
    || fail "missing-${missing_command}: command verification is missing"
done

if grep -Fq -- '--fix-missing' "${INSTALLER}"; then
  fail "installer must not bypass package errors with --fix-missing"
fi
if grep -Fq '|| true' "${INSTALLER}"; then
  fail "installer must not mask failures with || true"
fi
if grep -Fq -- '--foreground' "${INSTALLER}"; then
  fail "installer timeout must cover APT child processes"
fi

echo "native fixture installer regression tests passed"
