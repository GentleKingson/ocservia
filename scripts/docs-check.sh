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

require_text README.md 'docs/getting-started/production.md'
reject_text README.md '| bash -s'
reject_text docs/getting-started/production.md '| bash -s'
reject_text docs/getting-started/managed-node.md '| bash -s'
