#!/usr/bin/env bash
# Hosted-only coordination. Raw logs and decrypted tokens never leave RUNNER_TEMP.
set -Eeuo pipefail
umask 077
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROLE="${REAL_E2E_ROLE:?}"
[[ "$ROLE" == controller || "$ROLE" == node ]]
: "${RUN_ID:?}" "${RUNNER_TEMP:?}" "${GITHUB_RUN_ID:?}" "${GITHUB_RUN_ATTEMPT:?}"
[[ "$RUN_ID" == "${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}" ]]
PRIVATE="$RUNNER_TEMP/real-e2e-private-$RUN_ID-$ROLE"
EVIDENCE="$RUNNER_TEMP/real-e2e-evidence-$ROLE"
WORK="$RUNNER_TEMP/ocservia-real-e2e-$ROLE-$RUN_ID"
mkdir -p "$PRIVATE" "$EVIDENCE"
chmod 0700 "$PRIVATE" "$EVIDENCE"
export ARTIFACT_DIR="$PRIVATE/diagnostics"

private_run() {
  local phase="$1"
  shift
  if timeout --signal=TERM --kill-after=30s 35m "$@" >"$PRIVATE/$phase.log" 2>&1; then
    echo "completed private phase: $phase"
  else
    local status=$?
    echo "private phase $phase failed (exit $status); raw log withheld" >&2
    return "$status"
  fi
}

preflight() {
  [[ "$GITHUB_ACTIONS" == true && "$RUNNER_ENVIRONMENT" == github-hosted ]]
  # shellcheck source=/dev/null
  source /etc/os-release
  [[ "$ID" == ubuntu && "$VERSION_ID" == 24.04 && "$(uname -m)" == x86_64 ]]
  [[ "$(cat /proc/1/comm)" == systemd ]]
  [[ ! -e /.dockerenv && ! -e /run/.containerenv ]]
  for command in docker jq openssl curl unzip python3 ip timeout; do command -v "$command" >/dev/null; done
  systemctl show --property=Version >/dev/null
  docker info >/dev/null
  docker compose version >/dev/null
  python3 - "$EVIDENCE/environment.json" "$ROLE" <<'PY'
import json, os, pathlib, socket, subprocess, sys
p = pathlib.Path
out = {'role':sys.argv[2], 'boot_id':p('/proc/sys/kernel/random/boot_id').read_text().strip(),
       'hostname':socket.gethostname(), 'pid1':p('/proc/1/comm').read_text().strip(),
       'network_namespace':os.readlink('/proc/self/ns/net'),
       'default_routes':json.loads(subprocess.check_output(['ip','-json','route','show','default'])),
       'runner_os':'ubuntu-24.04', 'runner_environment':os.environ['RUNNER_ENVIRONMENT'],
       'source_commit':subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip(),
       'docker':subprocess.check_output(['docker','version','--format','{{.Server.Version}}'],text=True).strip(),
       'compose':subprocess.check_output(['docker','compose','version','--short'],text=True).strip()}
p(sys.argv[1]).write_text(json.dumps(out,indent=2)+'\n')
PY
}

case "${1:-}" in
  preflight) preflight ;;
  wait-download)
    # Confine even helper failure/timeout scratch archives to our private tree.
    RUNNER_TEMP="$PRIVATE" timeout --kill-after=10s 42m bash "$ROOT/scripts/real-e2e-artifact.sh" \
      wait-download "${2:?artifact name}" "$PRIVATE/${3:?destination}" 2400 "${4:?peer job}"
    ;;
  start)
    private_run start bash "$ROOT/scripts/real-e2e-controller.sh" start
    cp "$EVIDENCE/environment.json" "$WORK/outbox/controller-ready/environment.json"
    ;;
  build)
    private_run bootstrap bash "$ROOT/scripts/bootstrap.sh" native
    # shellcheck source=scripts/env.sh
    source "$ROOT/scripts/env.sh"
    private_run build bash "$ROOT/scripts/real-e2e-node.sh" build
    ;;
  prepare)
    python3 "$ROOT/scripts/real-e2e-evidence.py" "$EVIDENCE/environment.json" "$PRIVATE/controller-ready/environment.json"
    private_run prepare bash "$ROOT/scripts/real-e2e-node.sh" prepare "$PRIVATE/controller-ready"
    cp "$EVIDENCE/environment.json" "$WORK/outbox/agent-endpoint/environment.json"
    openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "$PRIVATE/exchange.key" 2>"$PRIVATE/keygen.log"
    openssl pkey -in "$PRIVATE/exchange.key" -pubout -out "$WORK/outbox/agent-endpoint/exchange.pub" 2>/dev/null
    ;;
  issue-token)
    python3 "$ROOT/scripts/real-e2e-evidence.py" "$EVIDENCE/environment.json" "$PRIVATE/agent-endpoint/environment.json"
    cp "$PRIVATE/agent-endpoint/environment.json" "$EVIDENCE/peer-environment.json"
    private_run issue-token bash "$ROOT/scripts/real-e2e-controller.sh" issue-token "$PRIVATE/agent-endpoint"
    mkdir -p "$PRIVATE/encrypted-token"
    openssl pkeyutl -encrypt -pubin -inkey "$PRIVATE/agent-endpoint/exchange.pub" \
      -pkeyopt rsa_padding_mode:oaep -pkeyopt rsa_oaep_md:sha256 -pkeyopt rsa_mgf1_md:sha256 \
      -in "$WORK/outbox/enrollment-token/enrollment-token" -out "$PRIVATE/encrypted-token/token.oaep"
    ;;
  enroll)
    mkdir -p "$PRIVATE/decrypted-token"
    openssl pkeyutl -decrypt -inkey "$PRIVATE/exchange.key" \
      -pkeyopt rsa_padding_mode:oaep -pkeyopt rsa_oaep_md:sha256 -pkeyopt rsa_mgf1_md:sha256 \
      -in "$PRIVATE/encrypted-token/token.oaep" -out "$PRIVATE/decrypted-token/enrollment-token"
    private_run enroll bash "$ROOT/scripts/real-e2e-node.sh" enroll "$PRIVATE/decrypted-token"
    rm -f "$PRIVATE/decrypted-token/enrollment-token" "$PRIVATE/exchange.key"
    ;;
  verify)
    private_run verify bash "$ROOT/scripts/real-e2e-controller.sh" verify-enrollment "$PRIVATE/enrollment-result"
    cp "$ARTIFACT_DIR/result.txt" "$EVIDENCE/enrollment.txt"
    cp "$ARTIFACT_DIR/binding.json" "$EVIDENCE/binding.json"
    cp "$PRIVATE/enrollment-result/agent-pid" "$EVIDENCE/agent-pid.txt"
    docker ps --filter "label=com.docker.compose.project=$COMPOSE_PROJECT" --format '{{.ID}}' >"$PRIVATE/container-ids"
    while read -r container; do
      docker inspect --format '{{json .State.Pid}} {{json .NetworkSettings.Networks}}' "$container"
    done <"$PRIVATE/container-ids" >"$EVIDENCE/controller-process-network.txt"
    ;;
  cleanup)
    trap 'rm -rf -- "$PRIVATE"' EXIT
    trap 'exit 143' TERM
    status=0
    # An outer deadline prevents a broken Docker daemon from holding cleanup open.
    private_run cleanup bash "$ROOT/scripts/real-e2e-$ROLE.sh" cleanup || status=$?
    if [[ "$ROLE" == controller ]]; then
      remaining="$(docker ps -aq --filter "label=com.docker.compose.project=$COMPOSE_PROJECT")" || status=1
      [[ -z "$remaining" ]] || status=1
      remaining="$(docker volume ls -q --filter "label=com.docker.compose.project=$COMPOSE_PROJECT")" || status=1
      [[ -z "$remaining" ]] || status=1
      remaining="$(docker network ls -q --filter "label=com.docker.compose.project=$COMPOSE_PROJECT")" || status=1
      [[ -z "$remaining" ]] || status=1
    fi
    rm -rf -- "$PRIVATE"
    [[ ! -e "$WORK" && ! -e "$PRIVATE" ]] || status=1
    [[ "$ROLE" != node || ! -e "$RUNNER_TEMP/ocservia-real-e2e-target-$RUN_ID" ]] || status=1
    ((status == 0))
    printf 'CLEANUP=PASS\n' >"$EVIDENCE/cleanup.txt"
    ;;
  *) echo 'unknown hosted phase' >&2; exit 2 ;;
esac
