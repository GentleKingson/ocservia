#!/bin/sh
set -eu

target=${1:-}
expected_uid=${2:-}
expected_gid=${3:-}
legacy_uid=${4:-}

case "${target}" in
  /run/ocserv-platform) ;;
  *)
    echo "transport runtime path must be /run/ocserv-platform" >&2
    exit 2
    ;;
esac
for value in "${expected_uid}" "${expected_gid}" "${legacy_uid}"; do
  case "${value}" in
    ''|*[!0-9]*)
      echo "transport runtime identities must be numeric" >&2
      exit 2
      ;;
  esac
done
if [ "$(id -u)" != 0 ]; then
  echo "transport runtime preparation must run as root" >&2
  exit 2
fi
if [ -L "${target}" ] || [ ! -d "${target}" ]; then
  echo "transport runtime must be a real directory" >&2
  exit 2
fi

if [ -L /run ]; then
  echo "transport runtime parent must not be a symbolic link" >&2
  exit 2
fi
parent_metadata=$(stat -c '%F:%u:%g:%a' -- /run)
case "${parent_metadata}" in
  directory:0:0:755|directory:0:0:750) ;;
  *)
    echo "transport runtime parent metadata is not trusted" >&2
    exit 2
    ;;
esac

identity=$(stat -c '%d:%i' -- "${target}")
metadata=$(stat -c '%F:%u:%g:%a' -- "${target}")
if [ "${metadata}" != "directory:${expected_uid}:${expected_gid}:750" ] \
  && [ "${metadata}" != "directory:${legacy_uid}:${expected_gid}:770" ] \
  && [ "${metadata}" != directory:0:0:700 ] \
  && [ "${metadata}" != directory:0:0:750 ] \
  && [ "${metadata}" != directory:0:0:770 ]; then
  echo "transport runtime metadata is neither current nor an approved legacy state: ${metadata}; expected=${expected_uid}:${expected_gid}; legacy=${legacy_uid}:${expected_gid}" >&2
  exit 2
fi

# Seize the mount root before inspecting its contents. This removes legacy
# shared-group write access before any pathname is trusted or removed.
chown 0:0 -- "${target}"
chmod 0700 -- "${target}"
if [ "$(stat -c '%d:%i' -- "${target}")" != "${identity}" ] || [ -L "${target}" ]; then
  echo "transport runtime identity changed during preparation" >&2
  exit 2
fi

unexpected=$(find "${target}" -mindepth 1 -maxdepth 1 ! -name transportd.sock -printf x -quit)
if [ -n "${unexpected}" ]; then
  echo "transport runtime contains an unexpected entry" >&2
  exit 2
fi
socket=${target}/transportd.sock
if [ -L "${socket}" ]; then
  echo "transport runtime socket must not be a symbolic link" >&2
  exit 2
fi
if [ -e "${socket}" ]; then
  socket_metadata=$(stat -c '%F:%u:%g:%a' -- "${socket}")
  case "${socket_metadata}" in
    "socket:${expected_uid}:${expected_gid}:660"|"socket:${legacy_uid}:${expected_gid}:660") ;;
    *)
      echo "transport runtime contains an untrusted socket entry" >&2
      exit 2
      ;;
  esac
  rm -- "${socket}"
fi
if [ -n "$(find "${target}" -mindepth 1 -maxdepth 1 -printf x -quit)" ]; then
  echo "transport runtime could not be emptied" >&2
  exit 2
fi

chown "${expected_uid}:${expected_gid}" -- "${target}"
chmod 0750 -- "${target}"
if [ "$(stat -c '%d:%i' -- "${target}")" != "${identity}" ] \
  || [ "$(stat -c '%F:%u:%g:%a' -- "${target}")" != "directory:${expected_uid}:${expected_gid}:750" ] \
  || [ -L "${target}" ]; then
  echo "transport runtime final metadata validation failed" >&2
  exit 2
fi
