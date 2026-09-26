#!/usr/bin/env bash
# Non-authoritative diagnostics for hosted G6 jobs.  The data is deliberately
# kept outside the evidence/verdict inputs so timing failures cannot affect a
# readiness decision.
set -euo pipefail

# Timings are diagnostic only. A filesystem, jq, or clock problem must never
# turn a readiness result into a pass or a failure; the calling workflow keeps
# running its authoritative work after this helper returns successfully.
if [[ "${G6_TIMING_REQUIRED:-false}" != true ]]; then
  trap 'echo "non-authoritative G6 timing collection failed" >&2; exit 0' ERR
fi

usage() {
  echo "usage: $0 <init|start|end|measure|artifact|image|rendezvous|rendezvous-dir|render|summary> ..." >&2
  exit 2
}

now_ms() {
  if [[ -r /proc/uptime ]]; then
    awk '{ printf "%.0f", $1 * 1000 }' /proc/uptime
  else
    node -p 'String(Number(process.hrtime.bigint() / 1000000n))'
  fi
}

append() {
  local file="${1:?timing file is required}"
  shift
  mkdir -p "$(dirname "${file}")"
  printf '%b\n' "$*" >>"${file}.tsv"
}

render() {
  local file="${1:?timing file is required}"
  [[ -f "${file}.tsv" ]] || return 0
  jq -Rn '
    reduce (inputs | split("\t")) as $row
      ({metadata: {}, stages: [], artifact_bytes: {}, images: {}, rendezvous: {}};
       if $row[0] == "meta" then .metadata[$row[1]] = $row[2]
       elif $row[0] == "duration" then .stages += [{name: $row[1], duration_ms: ($row[2] | tonumber)}]
       elif $row[0] == "artifact" then .artifact_bytes[$row[1]] = ($row[2] | tonumber)
       elif $row[0] == "image" then .images[$row[1]] = {bytes: ($row[2] | tonumber), image_id: $row[3]}
       elif $row[0] == "rendezvous" then .rendezvous[$row[1]] = ($row[2] | tonumber)
       else . end)
    | .metadata as $m | del(.metadata)
    | . + {job: $m.job, profile: $m.profile, candidate_sha: $m.candidate_sha, run_id: $m.run_id, run_attempt: $m.run_attempt}
  ' "${file}.tsv" >"${file}"
}

command="${1:-}"
shift || true
case "${command}" in
  init)
    [[ $# -eq 6 ]] || usage
    file="$1"
    mkdir -p "$(dirname "${file}")"
    : >"${file}.tsv"
    append "${file}" "meta\tjob\t$2"
    append "${file}" "meta\tprofile\t$3"
    append "${file}" "meta\tcandidate_sha\t$4"
    append "${file}" "meta\trun_id\t$5"
    append "${file}" "meta\trun_attempt\t$6"
    render "${file}"
    ;;
  start)
    [[ $# -eq 2 ]] || usage
    append "$1" "start\t$2\t$(now_ms)"
    ;;
  end)
    [[ $# -eq 2 ]] || usage
    start="$(awk -F '\t' -v stage="$2" '$1 == "start" && $2 == stage { value=$3 } END { print value }' "${1}.tsv")"
    [[ "${start}" =~ ^[0-9]+$ ]] || { echo "missing timing start for $2" >&2; exit 1; }
    end="$(now_ms)"
    append "$1" "duration\t$2\t$((end - start))"
    render "$1"
    ;;
  artifact)
    [[ $# -eq 3 ]] || usage
    [[ -f "$3" ]] || { echo "timing artifact does not exist: $3" >&2; exit 1; }
    append "$1" "artifact\t$2\t$(wc -c <"$3" | tr -d '[:space:]')"
    render "$1"
    ;;
  image)
    [[ $# -eq 4 ]] || usage
    [[ "$3" =~ ^[0-9]+$ ]] || { echo "timing image bytes must be an integer: $3" >&2; exit 1; }
    [[ "$4" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "timing image id must be a sha256 digest: $4" >&2; exit 1; }
    append "$1" "image\t$2\t$3\t$4"
    render "$1"
    ;;
  measure)
    [[ $# -ge 3 ]] || usage
    file="$1"
    stage="$2"
    shift 2
    [[ "${1:-}" == "--" ]] || usage
    shift
    # Wrap one timed command (for example a single release image build) and
    # always propagate its exit status: timing collection must never mask a
    # build failure, and a failed build still records its duration. Disarm
    # the non-authoritative ERR trap and errexit BEFORE any timing work so a
    # broken clock cannot exit 0 and skip the wrapped authoritative command;
    # the wrapped command always runs, timestamps are validated before they
    # are recorded, and the exit status comes only from the wrapped command.
    # Appends stay raw so concurrent measures never rewrite the JSON
    # mid-flight; the parent renders after every measured command has been
    # waited on.
    trap - ERR
    set +e
    start_ms="$(now_ms)"
    start_ms_valid=0
    [[ "${start_ms}" =~ ^[0-9]+$ ]] && start_ms_valid=1
    "$@"
    command_status=$?
    end_ms="$(now_ms)"
    end_ms_valid=0
    [[ "${end_ms}" =~ ^[0-9]+$ ]] && end_ms_valid=1
    if [[ "${start_ms_valid}" -eq 1 ]]; then
      append "${file}" "start\t${stage}\t${start_ms}" || :
    fi
    if [[ "${start_ms_valid}" -eq 1 && "${end_ms_valid}" -eq 1 ]]; then
      append "${file}" "duration\t${stage}\t$(( end_ms - start_ms ))" || :
    fi
    exit "${command_status}"
    ;;
  rendezvous)
    [[ $# -eq 3 ]] || usage
    [[ "$2" =~ ^[0-9]+$ && "$3" =~ ^[0-9]+$ ]] || usage
    append "$1" "rendezvous\tcount\t$2"
    append "$1" "rendezvous\tcumulative_wait_ms\t$3"
    render "$1"
    ;;
  rendezvous-dir)
    [[ $# -eq 2 ]] || usage
    [[ -d "$2" ]] || { echo "rendezvous directory does not exist: $2" >&2; exit 1; }
    rendezvous_metrics="$(node - "$2" <<'NODE'
const fs = require('fs');
const path = require('path');
const root = process.argv[2];
const files = fs.readdirSync(root)
  .filter((name) => name.endsWith('.result.json'))
  .map((name) => path.join(root, name));
let cumulativeWaitMs = 0;
for (const file of files) {
  const result = JSON.parse(fs.readFileSync(file, 'utf8'));
  const startedAt = Date.parse(result.started_at);
  const completedAt = Date.parse(result.completed_at);
  if (!Number.isFinite(startedAt) || !Number.isFinite(completedAt) || completedAt < startedAt) {
    throw new Error(`invalid rendezvous timestamps in ${file}`);
  }
  cumulativeWaitMs += completedAt - startedAt;
}
process.stdout.write(`${files.length}\t${cumulativeWaitMs}`);
NODE
)"
    IFS=$'\t' read -r rendezvous_count rendezvous_wait_ms <<<"${rendezvous_metrics}"
    append "$1" "rendezvous\tcount\t${rendezvous_count}"
    append "$1" "rendezvous\tcumulative_wait_ms\t${rendezvous_wait_ms}"
    render "$1"
    ;;
  render)
    [[ $# -eq 1 ]] || usage
    render "$1"
    ;;
  summary)
    [[ $# -eq 1 ]] || usage
    render "$1"
    [[ -s "$1" ]] || exit 0
    {
      echo "### G6 timing diagnostics"
      echo "| Stage | Duration |"
      echo "|---|---:|"
      jq -r '.stages[] | "| \(.name) | \(.duration_ms) ms |"' "$1"
      echo
      echo "| Artifact | Bytes |"
      echo "|---|---:|"
      jq -r '.artifact_bytes | to_entries[]? | "| \(.key) | \(.value) |"' "$1"
      echo
      echo "| Release image | Bytes | Image ID |"
      echo "|---|---:|---|"
      jq -r '.images | to_entries[]? | "| \(.key) | \(.value.bytes) | \(.value.image_id) |"' "$1"
      echo
      echo "| Rendezvous metric | Value |"
      echo "|---|---:|"
      jq -r '.rendezvous | to_entries[]? | "| \(.key) | \(.value) |"' "$1"
    } >>"${GITHUB_STEP_SUMMARY:-/dev/null}"
    ;;
  *) usage ;;
esac
