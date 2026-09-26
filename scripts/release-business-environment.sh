#!/usr/bin/env bash
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" == github-hosted ]]; then
  exit 0
fi
# The BuildServer rehearsal uses its own systemd and Docker daemon, never the
# host socket or host PID/network namespace. The marker is provisioned outside it.
[[ "${INTEGRATED_DISPOSABLE_CONTAINER:-}" == true && "$EUID" == 0 && -f /.dockerenv ]]
[[ "$(systemd-detect-virt --container)" == docker && "$(ps -p 1 -o comm=)" == systemd ]]
[[ "$(stat -c '%u:%g:%a' /etc/ocservia-acceptance-container)" == 0:0:600 ]]
[[ "$(cat /etc/ocservia-acceptance-container)" == ocservia-integrated-disposable ]]
[[ "$(findmnt -n -o FSTYPE -T /var/lib/docker)" == overlay ]]
[[ "$(findmnt -n -o TARGET -T /var/run/docker.sock)" == /run ]]
[[ "$(docker context inspect --format '{{.Endpoints.docker.Host}}')" == unix:///var/run/docker.sock ]]
systemctl is-active --quiet docker
