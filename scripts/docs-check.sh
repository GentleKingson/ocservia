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
python3 - "${ROOT}" <<'EOF'
import os, re, subprocess, sys

root = sys.argv[1]
tracked = subprocess.check_output(["git", "-C", root, "ls-files", "*.md"], text=True).splitlines()
docs = [f for f in tracked if f in ("README.md", "AGENTS.md", "CLAUDE.md", "SECURITY.md") or f.startswith("docs/")]

def slugify(heading):
    h = heading.strip().lower()
    h = re.sub(r"[^a-z0-9 -]", "", h)
    return h.replace(" ", "-")

headings = {}
def file_headings(path):
    if path not in headings:
        hs = set()
        in_fence = False
        with open(os.path.join(root, path), encoding="utf-8") as fh:
            for line in fh:
                if re.match(r"^\s*(```|~~~)", line):
                    in_fence = not in_fence
                    continue
                if not in_fence and re.match(r"^#+\s+", line):
                    hs.add(slugify(re.sub(r"^#+\s+", "", line)))
        headings[path] = hs
    return headings[path]

errors = []
for doc in docs:
    in_fence = False
    with open(os.path.join(root, doc), encoding="utf-8") as fh:
        for lineno, line in enumerate(fh, 1):
            if re.match(r"^\s*(```|~~~)", line):
                in_fence = not in_fence
                continue
            if in_fence:
                continue
            for match in re.finditer(r"\]\(([^)]+)\)", line):
                target = match.group(1)
                if re.match(r"^[a-zA-Z][a-zA-Z0-9+.-]*:", target):
                    continue
                path_part, _, anchor = target.partition("#")
                resolved = doc if not path_part else os.path.normpath(os.path.join(os.path.dirname(doc), path_part))
                if not os.path.exists(os.path.join(root, resolved)):
                    errors.append(f"{doc}:{lineno} links to missing path: {target}")
                elif anchor and resolved.endswith(".md") and anchor not in file_headings(resolved):
                    errors.append(f"{doc}:{lineno} links to missing heading: {target}")

if errors:
    for err in errors:
        print(err, file=sys.stderr)
    sys.exit(1)
EOF

require_text README.md 'docs/operations/production-deployment.md'
require_text README.md 'docs/operations/agent-lifecycle.md'
reject_text README.md '| bash -s'
reject_text docs/operations/production-deployment.md '| bash -s'
reject_text docs/operations/agent-lifecycle.md '| bash -s'
