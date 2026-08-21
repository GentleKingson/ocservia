#!/usr/bin/env bash
set -euo pipefail

readonly APT_ACQUIRE_RETRIES=2
readonly APT_REQUEST_TIMEOUT_SECONDS=10
readonly APT_UPDATE_TIMEOUT_SECONDS=150
readonly APT_INSTALL_TIMEOUT_SECONDS=300
readonly APT_TIMEOUT_KILL_AFTER_SECONDS=10
readonly APT_UPDATE_ERROR_MODE=any

APT_ETC_DIR="${OCSERVIA_CI_APT_ETC_DIR:-/etc/apt}"
APT_GET_BIN="${OCSERVIA_CI_APT_GET_BIN:-apt-get}"
TIMEOUT_BIN="${OCSERVIA_CI_TIMEOUT_BIN:-timeout}"
APT_SOURCES_LIST="${APT_ETC_DIR}/sources.list"
APT_SOURCES_DIR="${APT_ETC_DIR}/sources.list.d"
APT_MIRROR_LIST="${APT_ETC_DIR}/apt-mirrors.txt"
TEMP_BASE="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"

shopt -s nullglob

if [[ "${APT_ETC_DIR}" != /* ]]; then
  echo "native-fixtures: APT configuration directory must be absolute" >&2
  exit 1
fi
if [[ "${APT_ETC_DIR}" == "/etc/apt" && "${EUID}" -ne 0 ]]; then
  echo "native-fixtures: run this installer as root" >&2
  exit 1
fi
if [[ "${TEMP_BASE}" != /* || ! -d "${TEMP_BASE}" ]]; then
  echo "native-fixtures: temporary directory must be an existing absolute path" >&2
  exit 1
fi
if [[ -L "${APT_SOURCES_LIST}" || -L "${APT_SOURCES_DIR}" || -L "${APT_MIRROR_LIST}" ]]; then
  echo "native-fixtures: symbolic APT source paths are not supported" >&2
  exit 1
fi
for source_file in "${APT_SOURCES_DIR}"/*.list "${APT_SOURCES_DIR}"/*.sources; do
  if [[ -L "${source_file}" ]]; then
    echo "native-fixtures: symbolic APT source files are not supported" >&2
    exit 1
  fi
done
for required_command in "${APT_GET_BIN}" "${TIMEOUT_BIN}" awk cat cmp cp date grep rm touch; do
  if ! command -v "${required_command}" >/dev/null 2>&1; then
    echo "native-fixtures: required command not found: ${required_command}" >&2
    exit 1
  fi
done

readonly -a APT_OPTIONS=(
  -o "Acquire::Retries=${APT_ACQUIRE_RETRIES}"
  -o "Acquire::http::Timeout=${APT_REQUEST_TIMEOUT_SECONDS}"
  -o "Acquire::https::Timeout=${APT_REQUEST_TIMEOUT_SECONDS}"
)

backup_root=""
cleanup_unmodified_snapshot() {
  local original_status=$?
  local cleanup_status=0
  trap - EXIT

  if [[ -n "${backup_root}" && -d "${backup_root}" ]]; then
    rm -rf -- "${backup_root}" || cleanup_status=1
  fi
  if [[ "${original_status}" -eq 0 && "${cleanup_status}" -ne 0 ]]; then
    original_status="${cleanup_status}"
  fi
  exit "${original_status}"
}
trap cleanup_unmodified_snapshot EXIT

backup_root="$(mktemp -d "${TEMP_BASE%/}/ocservia-native-apt.XXXXXX")"
sources_list_present=0
sources_dir_present=0
mirror_list_present=0
sources_modified=0

if [[ -e "${APT_SOURCES_LIST}" || -L "${APT_SOURCES_LIST}" ]]; then
  cp -a -- "${APT_SOURCES_LIST}" "${backup_root}/sources.list"
  sources_list_present=1
fi
if [[ -e "${APT_SOURCES_DIR}" || -L "${APT_SOURCES_DIR}" ]]; then
  cp -a -- "${APT_SOURCES_DIR}" "${backup_root}/sources.list.d"
  sources_dir_present=1
fi
if [[ -e "${APT_MIRROR_LIST}" || -L "${APT_MIRROR_LIST}" ]]; then
  cp -a -- "${APT_MIRROR_LIST}" "${backup_root}/apt-mirrors.txt"
  mirror_list_present=1
fi

restore_sources() {
  local restore_status=0
  local backup_source
  local source_file

  if [[ "${sources_modified}" -eq 0 ]]; then
    return 0
  fi

  if [[ "${sources_list_present}" -eq 1 ]]; then
    if [[ ! -f "${APT_SOURCES_LIST}" || -L "${APT_SOURCES_LIST}" ]]; then
      restore_status=1
    else
      cat -- "${backup_root}/sources.list" >"${APT_SOURCES_LIST}" || restore_status=1
      touch -r "${backup_root}/sources.list" "${APT_SOURCES_LIST}" || restore_status=1
      cmp -s -- "${backup_root}/sources.list" "${APT_SOURCES_LIST}" || restore_status=1
    fi
  fi
  if [[ "${sources_dir_present}" -eq 1 ]]; then
    if [[ ! -d "${APT_SOURCES_DIR}" || -L "${APT_SOURCES_DIR}" ]]; then
      restore_status=1
    else
      for backup_source in \
        "${backup_root}/sources.list.d"/*.list \
        "${backup_root}/sources.list.d"/*.sources; do
        source_file="${APT_SOURCES_DIR}/${backup_source##*/}"
        if [[ ! -f "${source_file}" || -L "${source_file}" ]]; then
          restore_status=1
          continue
        fi
        cat -- "${backup_source}" >"${source_file}" || restore_status=1
        touch -r "${backup_source}" "${source_file}" || restore_status=1
        cmp -s -- "${backup_source}" "${source_file}" || restore_status=1
      done
    fi
  fi
  if [[ "${mirror_list_present}" -eq 1 ]]; then
    if [[ ! -f "${APT_MIRROR_LIST}" || -L "${APT_MIRROR_LIST}" ]]; then
      restore_status=1
    else
      cat -- "${backup_root}/apt-mirrors.txt" >"${APT_MIRROR_LIST}" || restore_status=1
      touch -r "${backup_root}/apt-mirrors.txt" "${APT_MIRROR_LIST}" || restore_status=1
      cmp -s -- "${backup_root}/apt-mirrors.txt" "${APT_MIRROR_LIST}" || restore_status=1
    fi
  fi

  return "${restore_status}"
}

cleanup() {
  local original_status=$?
  local cleanup_status=0
  trap - EXIT

  if ! restore_sources; then
    echo "native-fixtures: failed to restore the saved APT source state" >&2
    cleanup_status=1
  fi
  rm -rf -- "${backup_root}" || cleanup_status=1

  if [[ "${original_status}" -eq 0 && "${cleanup_status}" -ne 0 ]]; then
    original_status="${cleanup_status}"
  fi
  exit "${original_status}"
}
trap cleanup EXIT

APT_SOURCE_FILES=()
collect_source_files() {
  local source_file
  APT_SOURCE_FILES=()

  if [[ -f "${APT_SOURCES_LIST}" ]]; then
    APT_SOURCE_FILES+=("${APT_SOURCES_LIST}")
  fi
  if [[ -d "${APT_SOURCES_DIR}" ]]; then
    for source_file in "${APT_SOURCES_DIR}"/*.list "${APT_SOURCES_DIR}"/*.sources; do
      if [[ -f "${source_file}" ]]; then
        APT_SOURCE_FILES+=("${source_file}")
      fi
    done
  fi
  if [[ -f "${APT_MIRROR_LIST}" ]]; then
    APT_SOURCE_FILES+=("${APT_MIRROR_LIST}")
  fi
}

source_label() {
  local source_file

  collect_source_files
  for source_file in "${APT_SOURCE_FILES[@]}"; do
    if grep -Fq 'azure.archive.ubuntu.com' "${source_file}"; then
      printf '%s\n' 'azure.archive.ubuntu.com'
      return
    fi
  done
  for source_file in "${APT_SOURCE_FILES[@]}"; do
    if grep -Fq '://archive.ubuntu.com/ubuntu' "${source_file}"; then
      printf '%s\n' 'archive.ubuntu.com'
      return
    fi
  done
  printf '%s\n' 'runner-configured'
}

rewrite_index=0
rewrite_azure_source() {
  local source_file="$1"
  local deduplicate="$2"
  local rewritten

  rewrite_index=$((rewrite_index + 1))
  rewritten="${backup_root}/rewritten-${rewrite_index}"
  if [[ "${deduplicate}" -eq 1 ]]; then
    awk '
      {
        gsub(/https?:\/\/azure[.]archive[.]ubuntu[.]com\/ubuntu\/?/, "https://archive.ubuntu.com/ubuntu")
        if (!seen[$0]++) print
      }
    ' "${source_file}" >"${rewritten}"
  else
    awk '
      {
        gsub(/https?:\/\/azure[.]archive[.]ubuntu[.]com\/ubuntu\/?/, "https://archive.ubuntu.com/ubuntu")
        print
      }
    ' "${source_file}" >"${rewritten}"
  fi
  cat -- "${rewritten}" >"${source_file}"
}

switch_to_official_archive() {
  local canonical_found=0
  local source_file

  collect_source_files
  if [[ "${#APT_SOURCE_FILES[@]}" -eq 0 ]]; then
    echo "native-fixtures: no APT source files were found for fallback" >&2
    return 1
  fi

  sources_modified=1
  for source_file in "${APT_SOURCE_FILES[@]}"; do
    if [[ "${source_file}" == "${APT_MIRROR_LIST}" ]]; then
      rewrite_azure_source "${source_file}" 1
    else
      rewrite_azure_source "${source_file}" 0
    fi
  done

  collect_source_files
  for source_file in "${APT_SOURCE_FILES[@]}"; do
    if grep -Fq 'azure.archive.ubuntu.com' "${source_file}"; then
      echo "native-fixtures: Azure Ubuntu mirror remained after fallback switch" >&2
      return 1
    fi
    if grep -Fq '://archive.ubuntu.com/ubuntu' "${source_file}"; then
      canonical_found=1
    fi
  done
  if [[ "${canonical_found}" -ne 1 ]]; then
    echo "native-fixtures: official archive.ubuntu.com source is unavailable" >&2
    return 1
  fi

  echo "native-fixtures: fallback mirror=archive.ubuntu.com"
}

elapsed_seconds() {
  local started_at="$1"
  local finished_at
  local elapsed

  finished_at="$(date +%s)"
  elapsed=$((finished_at - started_at))
  if [[ "${elapsed}" -lt 0 ]]; then
    elapsed=0
  fi
  printf '%s\n' "${elapsed}"
}

run_apt_update() {
  local attempt="$1"
  local mirror="$2"
  local started_at
  local status

  started_at="$(date +%s)"
  if "${TIMEOUT_BIN}" --signal=TERM \
    "--kill-after=${APT_TIMEOUT_KILL_AFTER_SECONDS}s" \
    "${APT_UPDATE_TIMEOUT_SECONDS}s" \
    "${APT_GET_BIN}" "${APT_OPTIONS[@]}" \
    -o "APT::Update::Error-Mode=${APT_UPDATE_ERROR_MODE}" update; then
    status=0
  else
    status=$?
  fi
  printf 'native-fixtures: apt-update attempt=%s mirror=%s exit_code=%s elapsed_seconds=%s\n' \
    "${attempt}" "${mirror}" "${status}" "$(elapsed_seconds "${started_at}")"
  return "${status}"
}

run_apt_install() {
  local mirror="$1"
  local started_at
  local status

  started_at="$(date +%s)"
  if DEBIAN_FRONTEND=noninteractive \
    "${TIMEOUT_BIN}" --signal=TERM \
    "--kill-after=${APT_TIMEOUT_KILL_AFTER_SECONDS}s" \
    "${APT_INSTALL_TIMEOUT_SECONDS}s" \
    "${APT_GET_BIN}" "${APT_OPTIONS[@]}" install --yes \
    ocserv openconnect ssl-cert; then
    status=0
  else
    status=$?
  fi
  printf 'native-fixtures: apt-install attempt=1 mirror=%s exit_code=%s elapsed_seconds=%s\n' \
    "${mirror}" "${status}" "$(elapsed_seconds "${started_at}")"
  return "${status}"
}

echo "native-fixtures: saved current APT source state"
active_mirror="$(source_label)"
if run_apt_update 1 "${active_mirror}"; then
  update_status=0
else
  update_status=$?
fi

if [[ "${update_status}" -ne 0 ]]; then
  echo "native-fixtures: primary update failed; switching to archive.ubuntu.com"
  switch_to_official_archive
  active_mirror="archive.ubuntu.com"
  if run_apt_update 2 "${active_mirror}"; then
    update_status=0
  else
    update_status=$?
  fi
  if [[ "${update_status}" -ne 0 ]]; then
    echo "native-fixtures: fallback update failed closed" >&2
    exit "${update_status}"
  fi
fi

if run_apt_install "${active_mirror}"; then
  install_status=0
else
  install_status=$?
fi
if [[ "${install_status}" -ne 0 ]]; then
  echo "native-fixtures: package installation failed closed" >&2
  exit "${install_status}"
fi

for installed_command in ocserv ocpasswd openconnect; do
  if ! command -v "${installed_command}" >/dev/null 2>&1; then
    echo "native-fixtures: installed command is unavailable: ${installed_command}" >&2
    exit 1
  fi
  echo "native-fixtures: verified command=${installed_command}"
done

echo "native-fixtures: native fixture installation complete"
