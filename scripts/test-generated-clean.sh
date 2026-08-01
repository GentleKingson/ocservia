#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
temporary="$(mktemp -d)"
trap 'rm -rf "${temporary}"' EXIT INT TERM

git -C "${temporary}" init -q
git -C "${temporary}" config user.email generated-clean@invalid
git -C "${temporary}" config user.name generated-clean-test
mkdir -p "${temporary}/scripts" "${temporary}/control-plane/gen/proto"
cp "${ROOT}/scripts/generated-clean.sh" "${temporary}/scripts/"
cat >"${temporary}/scripts/generate.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
printf '%s\n' '// generated' >"${root}/control-plane/gen/proto/new.pb.go"
EOF
chmod +x "${temporary}/scripts/generate.sh"
git -C "${temporary}" add scripts
git -C "${temporary}" commit -qm baseline

if "${temporary}/scripts/generated-clean.sh" >/dev/null 2>&1; then
  echo "generated-clean accepted an untracked generated file" >&2
  exit 1
fi
