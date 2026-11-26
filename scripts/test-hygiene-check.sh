#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPORT_ONLY=false
if [[ "${1:-}" == "--report-only" ]]; then
  REPORT_ONLY=true
elif (($# > 0)); then
  echo "usage: $0 [--report-only]" >&2
  exit 2
fi

python3 - "${ROOT}" "${REPORT_ONLY}" <<'PY'
import os
import re
import subprocess
import sys

root = sys.argv[1]
report_only = sys.argv[2] == "true"

MAX_RATIO = 0.45
MAX_LINES = 400
DB_ENV_RE = re.compile(r"OCSERV_TEST_(?:OWNER_|INITIALIZATION_)?DATABASE_URL")
TEST_SCRIPT_RE = re.compile(r"^(?:test-|.*-(?:smoke|acceptance|integration|e2e|resilience|probe|scenarios)|perf-bench|required-go-tests)")

files = subprocess.check_output(["git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard"], text=True).splitlines()
prod_lines = 0
test_lines = 0
violations = []

for rel in files:
    path = os.path.join(root, rel)
    if not os.path.isfile(path):
        continue
    if "/gen/" in f"/{rel}" or "/generated/" in f"/{rel}":
        continue
    try:
        text = open(path, encoding="utf-8", errors="ignore").read()
    except OSError:
        continue
    lines = text.splitlines()
    nlines = len(lines)

    if rel.endswith(".go"):
        if rel.endswith("_test.go"):
            test_lines += nlines
            if nlines > MAX_LINES:
                violations.append(f"file exceeds {MAX_LINES} lines ({nlines}): {rel}")
            has_int_tag = bool(lines and re.match(r"^//go:build\s+.*\bintegration\b", lines[0]))
            if DB_ENV_RE.search(text) and not has_int_tag:
                violations.append(f"missing '//go:build integration' on line 1: {rel}")
            if not has_int_tag and "synctest.Test" not in text:
                for idx, line in enumerate(lines, 1):
                    if "time.Sleep(" in line and "// synctest" not in line:
                        violations.append(f"forbidden unit time.Sleep at {rel}:{idx}")
        else:
            prod_lines += nlines
    elif rel.endswith(".rs"):
        is_test_file = "/tests/" in f"/{rel}" or rel.startswith("rust/crates/test-support/") or rel.endswith(("_test.rs", "_tests.rs", "/tests.rs"))
        if is_test_file:
            test_lines += nlines
            if nlines > MAX_LINES:
                violations.append(f"file exceeds {MAX_LINES} lines ({nlines}): {rel}")
            for idx, line in enumerate(lines, 1):
                if ("std::thread::sleep(" in line or "tokio::time::sleep(" in line) and "// virtual-time" not in line:
                    violations.append(f"forbidden unit sleep at {rel}:{idx}")
        else:
            inline_test_ranges = []
            i = 0
            while i < nlines:
                if re.match(r"^\s*#\[cfg\(test\)\]", lines[i]):
                    j = i
                    while j < nlines and (
                        re.match(r"^\s*#\[", lines[j]) or lines[j].strip() == ""
                    ):
                        j += 1
                    if j < nlines and re.match(r"^(?:pub(?:\([^)]*\))?\s+)?mod\s+\w+\s*\{", lines[j].strip()):
                        depth = 0
                        k = j
                        while k < nlines:
                            depth += lines[k].count("{") - lines[k].count("}")
                            if depth <= 0:
                                break
                            k += 1
                        inline_test_ranges.append((i, min(k, nlines - 1)))
                        i = k + 1
                        continue
                i += 1
            if inline_test_ranges:
                inline_lines = sum(end - start + 1 for start, end in inline_test_ranges)
                prod_lines += max(0, nlines - inline_lines)
                test_lines += inline_lines
                if inline_lines > MAX_LINES:
                    violations.append(f"inline #[cfg(test)] exceeds {MAX_LINES} lines ({inline_lines}): {rel}")
                for start, end in inline_test_ranges:
                    for offset in range(start, end + 1):
                        if ("std::thread::sleep(" in lines[offset] or "tokio::time::sleep(" in lines[offset]) and "// virtual-time" not in lines[offset]:
                            violations.append(f"forbidden unit sleep at {rel}:{offset + 1}")
            else:
                prod_lines += nlines
    elif rel.startswith("web/") and rel.endswith((".ts", ".vue", ".js", ".mjs")):
        if rel.startswith(("web/test/", "web/e2e/")) or ".test." in rel or ".spec." in rel:
            test_lines += nlines
            if nlines > MAX_LINES:
                violations.append(f"file exceeds {MAX_LINES} lines ({nlines}): {rel}")
        else:
            prod_lines += nlines
    elif rel.startswith("scripts/") and rel.endswith((".sh", ".py", ".mjs", ".jq", ".txt")):
        bn = os.path.basename(rel)
        if TEST_SCRIPT_RE.match(bn):
            test_lines += nlines
        else:
            prod_lines += nlines

ratio = (test_lines / prod_lines) if prod_lines else 0.0
print(f"test-hygiene: prod={prod_lines} lines, test={test_lines} lines, ratio={ratio:.2f}x (limit={MAX_RATIO:.2f}x)")
if ratio > MAX_RATIO:
    violations.append(f"test-to-prod ratio {ratio:.2f}x exceeds {MAX_RATIO:.2f}x")

if violations:
    for item in violations:
        print(f"FAIL: {item}", file=sys.stderr)
    if not report_only:
        sys.exit(1)
PY
