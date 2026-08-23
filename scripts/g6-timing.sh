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
  echo "usage: $0 <init|start|end|artifact|rendezvous|rendezvous-dir|render|summary> ..." >&2
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
  local file="${1:?timing file is required}" metadata stages artifacts rendezvous
  [[ -f "${file}.tsv" ]] || return 0
  metadata="$(mktemp)"
  stages="$(mktemp)"
  artifacts="$(mktemp)"
  rendezvous="$(mktemp)"
  trap 'rm -f -- "${metadata}" "${stages}" "${artifacts}" "${rendezvous}"' RETURN
  awk -F '\t' '$1 == "meta" { print $2 "\t" $3 }' "${file}.tsv" >"${metadata}"
  awk -F '\t' '$1 == "duration" { print $2 "\t" $3 }' "${file}.tsv" >"${stages}"
  awk -F '\t' '$1 == "artifact" { print $2 "\t" $3 }' "${file}.tsv" >"${artifacts}"
  awk -F '\t' '$1 == "rendezvous" { print $2 "\t" $3 }' "${file}.tsv" >"${rendezvous}"
  jq -n \
    --slurpfile metadata <(jq -Rn '[inputs | split("\t") | {key: .[0], value: .[1]}]' <"${metadata}") \
    --slurpfile stages <(jq -Rn '[inputs | split("\t") | {name: .[0], duration_ms: (.[1] | tonumber)}]' <"${stages}") \
    --slurpfile artifacts <(jq -Rn '[inputs | split("\t") | {key: .[0], value: (.[1] | tonumber)}]' <"${artifacts}") \
    --slurpfile rendezvous <(jq -Rn '[inputs | split("\t") | {key: .[0], value: (.[1] | tonumber)}]' <"${rendezvous}") \
    '$metadata[0] | from_entries as $m | {job: $m.job, profile: $m.profile, candidate_sha: $m.candidate_sha, run_id: $m.run_id, run_attempt: $m.run_attempt, stages: $stages[0], artifact_bytes: ($artifacts[0] | from_entries), rendezvous: ($rendezvous[0] | from_entries)}' \
    >"${file}"
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
      echo "| Rendezvous metric | Value |"
      echo "|---|---:|"
      jq -r '.rendezvous | to_entries[]? | "| \(.key) | \(.value) |"' "$1"
    } >>"${GITHUB_STEP_SUMMARY:-/dev/null}"
    ;;
  *) usage ;;
esac
