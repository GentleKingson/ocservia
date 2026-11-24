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

work="$(mktemp -d)"
trap 'rm -rf -- "${work}"' EXIT

go_licenses_cmd=(go run github.com/google/go-licenses/v2@v2.0.1)
if command -v go-licenses >/dev/null 2>&1; then
  go_licenses_cmd=(go-licenses)
fi

run_go_licenses() {
  local module="$1"
  (
    cd "${ROOT}/${module}"
    GOWORK=off "${go_licenses_cmd[@]}" check ./... \
      --ignore="github.com/GentleKingson/ocservia/${module}" \
      --allowed_licenses=Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MIT
  )
}

run_go_licenses control-plane >"${work}/control-plane.log" 2>&1 &
cp_pid=$!
run_go_licenses signer >"${work}/signer.log" 2>&1 &
signer_pid=$!

cp_status=0
signer_status=0
wait "${cp_pid}" || cp_status=$?
wait "${signer_pid}" || signer_status=$?
cat "${work}/control-plane.log" "${work}/signer.log"
if ((cp_status != 0 || signer_status != 0)); then
  exit 1
fi

if [[ "${skip_rust}" != true ]]; then
  (cd "${ROOT}/rust" && cargo deny check licenses)
fi

python3 - "${ROOT}/web" <<'EOF'
import json
from pathlib import Path
import re
import sys

web_dir = Path(sys.argv[1])
lock = json.loads((web_dir / "package-lock.json").read_text(encoding="utf-8"))
disallowed = re.compile(r"AGPL|GPL|SSPL|BUSL|Elastic", re.IGNORECASE)
rejected = []
missing = []


def normalize_license(value):
    if isinstance(value, str):
        return value.strip()
    if isinstance(value, dict):
        return str(value.get("type") or "").strip()
    if isinstance(value, list):
        return ", ".join(filter(None, (normalize_license(item) for item in value)))
    return ""


for rel_path, meta in (lock.get("packages") or {}).items():
    if not rel_path.startswith("node_modules/"):
        continue
    license_text = normalize_license(meta.get("license"))
    name = meta.get("name") or rel_path.removeprefix("node_modules/")
    version = meta.get("version") or "unknown"
    if not license_text:
        manifest_path = web_dir / rel_path / "package.json"
        if manifest_path.is_file():
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            license_text = normalize_license(manifest.get("license"))
            name = manifest.get("name") or name
            version = manifest.get("version") or version
    if not license_text:
        missing.append(f"{name}@{version} ({rel_path})")
    elif disallowed.search(license_text):
        rejected.append(f"{name}@{version}: {license_text}")

if missing:
    raise SystemExit("missing dependency license metadata:\n" + "\n".join(missing))
if rejected:
    raise SystemExit("disallowed dependency licenses:\n" + "\n".join(rejected))
EOF
