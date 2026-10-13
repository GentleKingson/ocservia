#!/usr/bin/env python3
"""Remove repetitive repository-owner attribution from GitHub generated release notes.

Only modify GitHub's PR bullet form. Preserve other authors, PR links, headings,
manually written prose, and the Full Changelog.
"""
import re
import sys

OWNER = "GentleKingson"
PR_LINE = re.compile(
    rf"^(\s*[*-] .+?) by @{re.escape(OWNER)}"
    r"( in https://github\.com/GentleKingson/ocservia/pull/\d+\s*)$"
)


def clean(body: str) -> str:
    return "".join(PR_LINE.sub(r"\1\2", line) for line in body.splitlines(keepends=True))


if __name__ == "__main__":
    sys.stdout.write(clean(sys.stdin.read()))
