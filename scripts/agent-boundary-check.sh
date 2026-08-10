#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

privd_tree="$(cd "${ROOT}/rust" && cargo tree --locked --package ocservia-privd --prefix none)"
if grep -Eiq '^(iroh|reqwest|hyper|tonic|postgres|postgres-types|tokio-postgres|sqlx|diesel|sea-orm) ' <<<"${privd_tree}"; then
  echo "privd must not depend on remote networking, Iroh, gRPC, or remote databases" >&2
  exit 1
fi

if grep -R -n -E --include='*.rs' \
  'shell\.exec|command\.run|occtl\.raw|systemctl\.raw|journalctl\.raw|Command::new\([^)]*(request|input|argument)' \
  "${ROOT}/rust/crates/agent" "${ROOT}/rust/crates/privd" "${ROOT}/rust/crates/ocserv-adapter"; then
  echo "generic execution capability found in Agent privilege boundary" >&2
  exit 1
fi

if grep -R -n -E --include='*.go' --include='*.rs' \
  'Command::new\([^)]*(sh|bash)|exec\.Command\([^)]*(sh|bash)|docker\.sock' \
  "${ROOT}/control-plane" "${ROOT}/rust"; then
  echo "forbidden generic shell or Docker socket surface found" >&2
  exit 1
fi

grep -Fxq 'User=ocserv-agent' "${ROOT}/deploy/systemd/ocservia-agent.service"
grep -Fxq 'CapabilityBoundingSet=' "${ROOT}/deploy/systemd/ocservia-agent.service"
grep -Fxq 'CapabilityBoundingSet=CAP_DAC_OVERRIDE' "${ROOT}/deploy/systemd/ocservia-privd.service"
grep -Fxq 'EnvironmentFile=/etc/ocservia-agent/agent.env' "${ROOT}/deploy/systemd/ocservia-privd.service"
grep -Fxq 'ExecStart=/usr/libexec/ocservia/ocservia-privd --agent-uid $AGENT_UID --node-id $NODE_ID --controller-command-key-file $CONTROLLER_COMMAND_VERIFICATION_KEY_FILE' "${ROOT}/deploy/systemd/ocservia-privd.service"
grep -Fxq 'RestrictAddressFamilies=AF_UNIX AF_NETLINK' "${ROOT}/deploy/systemd/ocservia-privd.service"
grep -Fxq 'IPAddressDeny=any' "${ROOT}/deploy/systemd/ocservia-privd.service"
grep -Fxq 'StateDirectory=ocservia-privd' "${ROOT}/deploy/systemd/ocservia-privd.service"
grep -Fxq 'StateDirectoryMode=0710' "${ROOT}/deploy/systemd/ocservia-privd.service"
grep -Fxq 'ReadWritePaths=-/etc/ocserv /var/lib/ocservia-privd' "${ROOT}/deploy/systemd/ocservia-privd.service"
