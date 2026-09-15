#!/usr/bin/env bash
# Runs only inside a disposable, systemd-booted test container.
set -euo pipefail
[[ "$EUID" == 0 && -f /.dockerenv && -d /run/systemd/system ]] || {
  echo 'isolated root systemd Docker container required' >&2; exit 2;
}
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
[[ ! -e /usr/libexec/ocservia/ocservia-agent && ! -e /etc/ocservia-agent ]] || exit 2
cleanup() {
  systemctl stop ocservia-agent.service ocservia-privd.service || true
  systemctl reset-failed ocservia-agent.service ocservia-privd.service || true
  rm -f /etc/systemd/system/ocservia-agent.service /etc/systemd/system/ocservia-privd.service
  rm -rf /etc/systemd/system/ocservia-agent.service.d /etc/ocservia-agent /var/lib/ocservia-agent /usr/libexec/ocservia
  systemctl daemon-reload
  userdel ocserv-agent
}
trap cleanup EXIT
useradd -r -U ocserv-agent
install -d /usr/libexec/ocservia /etc/ocservia-agent /etc/systemd/system/ocservia-agent.service.d
install -d -o ocserv-agent -g ocserv-agent /var/lib/ocservia-agent
install -m 0755 "$ROOT/deploy/production/systemd/agent-relays.sh" /usr/libexec/ocservia/ocservia-agent-relays
install -m 0644 "$ROOT/deploy/systemd/ocservia-agent.service" /etc/systemd/system/ocservia-agent.service
install -m 0644 "$ROOT/deploy/production/systemd/ocservia-agent-relays.conf" /etc/systemd/system/ocservia-agent.service.d/10-production-relays.conf
printf '[Service]\nRestart=no\n' >/etc/systemd/system/ocservia-agent.service.d/90-test.conf
printf '[Service]\nType=oneshot\nExecStart=/bin/true\nRemainAfterExit=yes\n' >/etc/systemd/system/ocservia-privd.service
cat >/usr/libexec/ocservia/ocservia-agent <<'PY'
#!/usr/bin/python3
import json, os, signal, sys, time
with open("/var/lib/ocservia-agent/argv.json", "w") as output:
    json.dump({"argv": sys.argv[1:], "uid": os.getuid(), "gid": os.getgid()}, output)
signal.signal(signal.SIGTERM, lambda *_: sys.exit(23))
time.sleep(600)
PY
chmod 0755 /usr/libexec/ocservia/ocservia-agent
cat >/etc/ocservia-agent/agent.env <<'EOF'
CONTROLLER_ENDPOINT_ID=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
NODE_ID=018f1e11-2222-7333-8444-555555555555
CONTROLLER_COMMAND_VERIFICATION_KEY_FILE="/protected/a key's $literal.pem"
USER_PASSWORD_SEAL_KEY_ID=user
USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
P12_PASSWORD_SEAL_KEY_ID=p12
P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
EOF
systemctl daemon-reload
for b in unset empty dual; do
  printf 'RELAY_URL_A=https://relay-a.example.test\n' >/etc/ocservia-agent/relays.env
  case "$b" in empty) printf 'RELAY_URL_B=\n' >>/etc/ocservia-agent/relays.env ;;
    dual) printf 'RELAY_URL_B=https://relay-b.example.test\n' >>/etc/ocservia-agent/relays.env ;;
  esac
  rm -f /var/lib/ocservia-agent/argv.json
  systemctl reset-failed ocservia-agent.service || true
  systemctl start ocservia-agent.service
  for _ in {1..30}; do
    [[ ! -s /var/lib/ocservia-agent/argv.json ]] || break
    sleep 0.1
  done
  python3 - "$b" "$(id -u ocserv-agent)" "$(id -g ocserv-agent)" <<'PY'
import json, sys
with open("/var/lib/ocservia-agent/argv.json") as source:
    result = json.load(source)
argv = result["argv"]
assert result["uid"] == int(sys.argv[2]) and result["gid"] == int(sys.argv[3])
assert argv.count("--relay-url") == (2 if sys.argv[1] == "dual" else 1)
assert argv[argv.index("--controller-command-key-file") + 1] == "/protected/a key's $literal.pem"
assert argv[-2:] == ["--relay-token-file", "/etc/ocservia-agent/relay-access-token"]
assert "" not in argv
print(json.dumps(result))
PY
  systemctl stop ocservia-agent.service
  [[ "$(systemctl show ocservia-agent.service -p ExecMainStatus --value)" == 23 ]]
  echo "PASS systemd $b: literal argv, UID/GID, exec and SIGTERM exit status"
done
