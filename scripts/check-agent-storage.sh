#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

# Read-only monitoring probe. Exit codes follow OK/WARNING/CRITICAL/UNKNOWN.
unknown() { echo "UNKNOWN: $*" >&2; exit 3; }
trap 'unknown "storage probe failed at line ${LINENO}"' ERR
if (($# > 1)); then
  unknown "usage: $0 [/absolute/path/to/agent.db]"
fi
journal="${1:-/var/lib/ocservia-agent/agent.db}"
warning="${STORAGE_WARNING_PERCENT:-80}"
critical="${STORAGE_CRITICAL_PERCENT:-90}"
[[ "${warning}" =~ ^[1-9][0-9]?$ && "${critical}" =~ ^[1-9][0-9]?$ ]] \
  || unknown "thresholds must be integer percentages from 1 to 99"
((warning < critical)) || unknown "warning threshold must be below critical"
[[ "${journal}" == /* && -f "${journal}" && ! -L "${journal}" ]] \
  || unknown "journal must be an existing absolute regular non-symlink file"

total_bytes=0
allocated_bytes=0
for suffix in '' -wal -shm; do
  path="${journal}${suffix}"
  bytes=0
  blocks=0
  if [[ -e "${path}" || -L "${path}" ]]; then
    [[ -f "${path}" && ! -L "${path}" ]] || unknown "journal component is not a regular file"
    values="$(stat -c '%s %b' -- "${path}")"
    read -r bytes blocks <<<"${values}"
  fi
  total_bytes=$((total_bytes + bytes))
  allocated_bytes=$((allocated_bytes + blocks * 512))
  case "${suffix}" in
    '') name=database ;;
    -wal) name=wal ;;
    -shm) name=shm ;;
  esac
  printf '%s_bytes=%s\n' "${name}" "${bytes}"
done

directory="$(dirname -- "${journal}")"
disk="$(df -P -B1 -- "${directory}" | awk 'END {print $2, $4, $5}')"
inodes="$(df -Pi -- "${directory}" | awk 'END {print $2, $4, $5}')"
read -r disk_total disk_available disk_percent <<<"${disk}"
read -r inode_total inode_available inode_percent <<<"${inodes}"
disk_percent="${disk_percent%%%}"
inode_percent="${inode_percent%%%}"
for value in "${disk_total}" "${disk_available}" "${disk_percent}" "${inode_total}" "${inode_available}" "${inode_percent}"; do
  [[ "${value}" =~ ^[0-9]+$ ]] || unknown "filesystem statistics are unavailable"
done
((disk_total > 0 && inode_total > 0)) || unknown "filesystem capacity is unavailable"
printf 'observed_at=%s\njournal_bytes=%s\njournal_allocated_bytes=%s\n' \
  "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${total_bytes}" "${allocated_bytes}"
printf 'filesystem_bytes=%s\nfilesystem_available_bytes=%s\nfilesystem_used_percent=%s\n' \
  "${disk_total}" "${disk_available}" "${disk_percent}"
printf 'filesystem_inodes=%s\nfilesystem_available_inodes=%s\nfilesystem_used_inode_percent=%s\n' \
  "${inode_total}" "${inode_available}" "${inode_percent}"
if ((disk_percent >= critical || inode_percent >= critical)); then
  echo "CRITICAL: provision capacity; do not delete journal state"
  exit 2
elif ((disk_percent >= warning || inode_percent >= warning)); then
  echo "WARNING: review journal growth and filesystem capacity"
  exit 1
fi
echo "OK: journal storage is below capacity thresholds"
