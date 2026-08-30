#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

if (($# > 1)) || { (($# == 1)) && [[ "$1" != "--skip-rust" ]]; }; then
  echo "usage: $0 [--skip-rust]" >&2
  exit 2
fi
skip_rust=false
if (($# == 1)); then
  skip_rust=true
fi

(cd "${ROOT}/control-plane" && \
  go run github.com/google/go-licenses/v2@v2.0.1 check ./... \
    --ignore=github.com/GentleKingson/ocservia/control-plane \
    --allowed_licenses=Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MIT)

if [[ "${skip_rust}" != true ]]; then
  (cd "${ROOT}/rust" && cargo deny check licenses)
fi

(cd "${ROOT}/web" && node <<'EOF'
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

const lock = JSON.parse(readFileSync("package-lock.json", "utf8"));
const rejected = [];
for (const path of Object.keys(lock.packages ?? {})) {
  if (!path.startsWith("node_modules/")) continue;
  const manifestPath = join(path, "package.json");
  if (!existsSync(manifestPath)) continue;
  const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
  const license = String(manifest.license ?? "");
  if (/AGPL|GPL|SSPL|BUSL|Elastic/i.test(license)) {
    rejected.push(`${manifest.name}@${manifest.version}: ${license}`);
  }
}
if (rejected.length > 0) {
  throw new Error(`disallowed dependency licenses:\n${rejected.join("\n")}`);
}
EOF
)
