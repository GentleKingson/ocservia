#!/usr/bin/env bash
set -euo pipefail

PURGE_STATE=false
if [[ ${1:-} == "--purge-state" ]]; then
  PURGE_STATE=true
elif [[ $# -ne 0 ]]; then
  echo "usage: uninstall-agent.sh [--purge-state]" >&2
  exit 2
fi

if [[ ${EUID} -ne 0 ]]; then
  echo "uninstall-agent.sh must run as root" >&2
  exit 1
fi

systemctl disable --now ocservia-agent.service ocservia-privd.service 2>/dev/null || true
rm -f /usr/lib/systemd/system/ocservia-agent.service /usr/lib/systemd/system/ocservia-privd.service
rm -f /usr/libexec/ocservia/ocservia-agent /usr/libexec/ocservia/ocservia-privd
rmdir /usr/libexec/ocservia 2>/dev/null || true
systemctl daemon-reload

if [[ ${PURGE_STATE} == true ]]; then
  rm -rf /var/lib/ocservia-agent /var/lib/ocservia-privd /etc/ocservia-agent
  userdel ocserv-agent 2>/dev/null || true
  groupdel ocserv-agent 2>/dev/null || true
fi
