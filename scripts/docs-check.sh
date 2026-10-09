#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

if git -C "${ROOT}" grep -I -n $'\r' -- '*.md' '*.yaml' '*.yml' '*.proto' '*.go' '*.rs' '*.ts'; then
  echo "CRLF line ending found" >&2
  exit 1
fi

while IFS= read -r file; do
  [[ -s "${ROOT}/${file}" ]] || {
    echo "empty public documentation file: ${file}" >&2
    exit 1
  }
done < <(git -C "${ROOT}" ls-files '*.md')

# CI routes shared Claude Code settings here instead of the product suites, so
# only the audited keys may pass. Hooks, status lines, helpers, env, MCP/plugins
# and permission modes run code or widen authority: keep them unknown until reviewed.
# Allow rules grant authority, so each must match a reviewed entry exactly; ask and
# deny rules only narrow it. Add a rule here only after reviewing what it permits.
reviewed_allow='[]'
settings="${ROOT}/.claude/settings.json"
if [[ -e "${settings}" ]] && ! jq -es --argjson reviewed "${reviewed_allow}" '
  def string_list: type == "array" and all(.[]; type == "string");
  length == 1 and (.[0] | type == "object" and
    (keys - ["attribution", "autoCompactWindow", "effortLevel", "language", "model", "permissions"] == []) and
    all(.model, .effortLevel, .language; . == null or type == "string") and
    (.autoCompactWindow == null or (.autoCompactWindow | type == "number")) and
    (.attribution == null or (.attribution | type == "object" and
      (keys - ["commit", "pr", "sessionUrl"] == []) and
      all(.commit, .pr; . == null or type == "string") and
      (.sessionUrl == null or (.sessionUrl | type == "boolean")))) and
    (.permissions == null or (.permissions | type == "object" and
      (keys - ["allow", "ask", "deny"] == []) and all(.[]; string_list) and
      ((.allow // []) - $reviewed == []))))' "${settings}" >/dev/null; then
  echo "unaudited or invalid Claude Code settings: .claude/settings.json" >&2
  exit 1
fi

require_text() {
  local file="$1" text="$2"
  grep -Fq -- "${text}" "${ROOT}/${file}" || {
    echo "required bootstrap documentation is missing from ${file}: ${text}" >&2
    exit 1
  }
}

reject_text() {
  local file="$1" text="$2"
  if grep -Fq -- "${text}" "${ROOT}/${file}"; then
    echo "forbidden bootstrap documentation found in ${file}: ${text}" >&2
    exit 1
  fi
}

# Agent entry: CLAUDE.md must import AGENTS.md as a body line (Claude Code only
# loads imports outside code), and AGENTS.md navigation targets must exist.
[[ -s "${ROOT}/AGENTS.md" ]] || { echo "AGENTS.md is missing or empty" >&2; exit 1; }
if [[ -e "${ROOT}/CLAUDE.md" ]] && ! awk '/^[[:space:]]*(```|~~~)/ { fence = !fence; next }
  !fence && $0 == "@AGENTS.md" { found = 1 } END { exit !found }' "${ROOT}/CLAUDE.md"; then
  echo "CLAUDE.md must import AGENTS.md with an @AGENTS.md body line" >&2
  exit 1
fi
# ponytail: inline relative links and ASCII GitHub heading slugs only (no
# duplicate-heading suffixes); use a Markdown parser if AGENTS.md needs more.
while IFS= read -r link; do
  path="${link%%#*}" anchor=""
  [[ "${link}" != *#* ]] || anchor="${link#*#}"
  [[ -f "${ROOT}/${path}" ]] || { echo "AGENTS.md links to a missing file: ${link}" >&2; exit 1; }
  [[ -z "${anchor}" ]] || awk -v want="${anchor}" '/^#+ / { h = tolower($0); sub(/^#+ /, "", h)
    gsub(/[^a-z0-9 -]/, "", h); gsub(/ /, "-", h); if (h == want) found = 1 } END { exit !found }' \
    "${ROOT}/${path}" || { echo "AGENTS.md links to a missing heading: ${link}" >&2; exit 1; }
done < <(grep -oE '\]\([^)]+\)' "${ROOT}/AGENTS.md" | sed -E 's/^\]\((.*)\)$/\1/' | grep -vE '^[a-z]+:' || true)

require_text README.md 'docs/getting-started/production.md'
reject_text README.md '| bash -s'
reject_text docs/getting-started/production.md '| bash -s'
reject_text docs/getting-started/managed-node.md '| bash -s'
