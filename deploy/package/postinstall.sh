#!/bin/sh
set -eu
systemd-sysusers ocservia-agent.conf
systemd-tmpfiles --create ocservia-agent.conf
env_file=/etc/ocservia-agent/privd.env
if [ ! -e "$env_file" ] && [ ! -L "$env_file" ]; then
  staged=$(mktemp "${env_file}.XXXXXX")
  trap 'rm -f -- "$staged"' EXIT
  trap 'exit 1' HUP INT TERM
  {
    printf 'AGENT_UID=%s\n' "$(id -u ocserv-agent)"
    echo 'PRIVD_ATTESTATION_KEY_FILE=/var/lib/ocservia-privd/attestation.key'
    echo 'USER_PASSWORD_SEAL_PRIVATE_KEY_FILE=/etc/ocservia-agent/user-password-seal-private.pem'
    echo 'P12_PASSWORD_SEAL_PRIVATE_KEY_FILE=/etc/ocservia-agent/p12-password-seal-private.pem'
  } >"$staged"
  chmod 0640 "$staged"
  ln -T -- "$staged" "$env_file"
fi
# Remove the two unowned helpers left by the v1 tar installer.
rm -f /usr/libexec/ocservia/ocservia-agent-verify /usr/libexec/ocservia/ocservia-agent-rollback
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload
  systemctl try-restart ocservia-privd.service ocservia-agent.service ocservia-network-audit.service
fi
