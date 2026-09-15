#!/usr/bin/env bash
set -euo pipefail
arch="${1:?architecture required}"
case "${arch}" in
  amd64) runner_arch=X64; kernel=x86_64 ;;
  arm64) runner_arch=ARM64; kernel=aarch64 ;;
  *) exit 2 ;;
esac
[[ "${RUNNER_ARCH:?RUNNER_ARCH required}" == "${runner_arch}" && "$(uname -m)" == "${kernel}" ]]
[[ "$(docker version --format '{{.Server.Arch}}')" == "${arch}" ]]
[[ "$(docker context inspect --format '{{.Endpoints.docker.Host}}')" == unix://* ]]
[[ -z "${DOCKER_HOST:-}" || "${DOCKER_HOST}" == unix://* ]]
for entry in /proc/sys/fs/binfmt_misc/*; do
  case "${entry##*/}" in register|status|'*') continue ;; esac
  if [[ -f "${entry}" ]] && grep -q '^enabled' "${entry}"; then
    echo "native validation refuses active binfmt handlers: ${entry}" >&2
    exit 1
  fi
done
jq -n --arg runner "${runner_arch}" --arg kernel "${kernel}" --arg docker "${arch}" \
  '{runner_arch:$runner,kernel:$kernel,docker:$docker,binfmt:"none"}'
