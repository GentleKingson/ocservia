#!/bin/sh
set -eu
case "${1:-}" in
  remove|0)
    if [ -d /run/systemd/system ]; then
      systemctl disable --now ocservia-agent.service ocservia-privd.service ocservia-network-audit.service
    fi
    ;;
esac
