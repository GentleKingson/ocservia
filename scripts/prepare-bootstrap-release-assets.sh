#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if (($# < 1 || $# > 2)); then
  echo "usage: prepare-bootstrap-release-assets.sh <asset-dir> [<release-tag>]" >&2
  exit 2
fi

asset_dir="$(realpath -- "$1")"
release_tag="${2-}"
if [[ $# -eq 2 && ! "${release_tag}" =~ ^v[0-9]+[.][0-9]+[.][0-9]+(-rc[.][1-9][0-9]*)?$ ]]; then
  echo "release tag must be vX.Y.Z or vX.Y.Z-rc.N: ${release_tag}" >&2
  exit 2
fi
[[ -d "${asset_dir}" ]] || {
  echo "bootstrap asset directory does not exist: ${asset_dir}" >&2
  exit 1
}

copy_asset() {
  local source="$1" name="$2"
  [[ -f "${source}" && ! -L "${source}" && -x "${source}" ]] || {
    echo "bootstrap source is missing, not executable, or a symlink: ${source}" >&2
    exit 1
  }
  if LC_ALL=C grep -q $'\r' "${source}"; then
    echo "bootstrap source must use LF line endings: ${source}" >&2
    exit 1
  fi
  install -m 0755 -- "${source}" "${asset_dir}/${name}"
}

copy_asset "${ROOT}/deploy/production/controller-bootstrap.sh" "controller-bootstrap.sh"
copy_asset "${ROOT}/deploy/managed-node/install.sh" "managed-node-bootstrap.sh"

# Embed shared helpers so each Stage-1 remains a standalone Release asset.
for asset in controller-bootstrap.sh managed-node-bootstrap.sh; do
  python3 - "${asset_dir}/${asset}" "${ROOT}" <<'EMBED'
from pathlib import Path
import sys
p, root = Path(sys.argv[1]), Path(sys.argv[2])
s = p.read_text()
for helper in ('install-ui', 'controller-runtime'):
    marker = f'source "${{SCRIPT_LIB}}/{helper}.sh" # release-embed'
    s = s.replace(marker, (root / f'deploy/lib/{helper}.sh').read_text())
p.write_text(s)
EMBED
done
if [[ -n "${release_tag}" ]]; then
  copy_asset "${ROOT}/deploy/bootstrap/install-controller" "install-controller"
  copy_asset "${ROOT}/deploy/bootstrap/install-node" "install-node"
  for asset in install-controller install-node controller-bootstrap.sh managed-node-bootstrap.sh; do
    [[ "$(grep -cxF 'readonly DEFAULT_VERSION=""' "${asset_dir}/${asset}")" == 1 ]] || {
      echo "${asset} must contain exactly one default version marker" >&2; exit 1;
    }
    sed -i "s|^readonly DEFAULT_VERSION=\"\"\$|readonly DEFAULT_VERSION=\"${release_tag}\"|" "${asset_dir}/${asset}"
  done
  "${ROOT}/scripts/check-release-bootstrap.sh" "${asset_dir}/install-controller" "${release_tag}"
  install -m 0644 "${ROOT}/install.env.example" "${asset_dir}/install.env.example"
  runtime="$(mktemp -d)"
  trap 'rm -rf -- "${runtime}"' EXIT
  # Only runtime scripts, Compose files and bind-mounted public configuration.
  files=(deploy/lib/install-env.sh deploy/lib/controller-runtime.sh deploy/lib/postgres-entrypoint.sh)
  for file in bootstrap-host.sh install.sh controller.sh compose.sh controller-release-smoke.sh quick-install.sh quick-materials.sh rotate-postgres-credentials.sh; do
    files+=("deploy/production/${file}")
  done
  # Expand globs at the repository root, independent of the caller's cwd.
  for pattern in 'deploy/production/compose*.yaml' 'deploy/production/integrated/compose*.yaml'; do
    while IFS= read -r file; do files+=("${file}"); done < <(cd "${ROOT}" && compgen -G "${pattern}")
  done
  files+=(deploy/production/Caddyfile deploy/production/postgresql.conf deploy/production/otel-collector.yaml
    deploy/production/integrated/Caddyfile deploy/production/integrated/Caddyfile.acme
    deploy/production/integrated/site.caddy deploy/production/integrated/relay.acme.toml
    deploy/production/relay/compose.sh deploy/production/relay/compose.yaml deploy/production/relay/relay.toml deploy/production/postgres-init/001-runtime-role.sh install.env.example)
  for file in "${files[@]}"; do
    [[ -n "${file}" ]] || continue
    mkdir -p "${runtime}/$(dirname "${file}")"
    cp -p "${ROOT}/${file}" "${runtime}/${file}"
  done
  printf '%s %s\n' "${release_tag}" "$(git -C "${ROOT}" rev-parse HEAD)" >"${runtime}/RELEASE"
  (cd "${runtime}" && find deploy install.env.example RELEASE -type f | LC_ALL=C sort | xargs sha256sum >SHA256SUMS)
  archive="controller-runtime-${release_tag}.tar.gz"
  (cd "${runtime}" && find deploy install.env.example RELEASE SHA256SUMS -type f | LC_ALL=C sort | tar -czf "${asset_dir}/${archive}" -T -)
  (cd "${asset_dir}" && sha256sum "${archive}" >"${archive}.sha256")
fi
