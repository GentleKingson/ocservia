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
printf '%s\n' '// generated' >"${temporary}/schema-source"
cat >"${temporary}/scripts/generate.sh" <<'GENERATOR'
#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cat "${root}/schema-source" >"${root}/control-plane/gen/proto/new.pb.go"
GENERATOR
chmod +x "${temporary}/scripts/generate.sh"
git -C "${temporary}" add scripts schema-source
git -C "${temporary}" commit -qm baseline

if "${temporary}/scripts/generated-clean.sh" >/dev/null 2>&1; then
  echo "generated-clean accepted an untracked generated file" >&2
  exit 1
fi
git -C "${temporary}" add control-plane/gen/proto/new.pb.go
git -C "${temporary}" commit -qm generated
"${temporary}/scripts/generated-clean.sh"

# A committed schema change must actually regenerate; merely comparing the
# initially clean tree to the index would incorrectly pass this case.
printf '%s\n' '// generated from changed schema' >"${temporary}/schema-source"
git -C "${temporary}" add schema-source
git -C "${temporary}" commit -qm schema-change
if "${temporary}/scripts/generated-clean.sh" >/dev/null 2>&1; then
  echo "generated-clean accepted stale generated code after a schema change" >&2
  exit 1
fi
git -C "${temporary}" add control-plane/gen/proto/new.pb.go
if "${temporary}/scripts/generated-clean.sh" --skip-generate >/dev/null 2>&1; then
  echo "read-only generated preflight accepted a staged generated difference" >&2
  exit 1
fi
printf '%s\n' '// user work' >"${temporary}/control-plane/gen/proto/new.pb.go"
if "${temporary}/scripts/generated-clean.sh" >/dev/null 2>&1; then
  echo "generated-clean regenerated in a dirty candidate checkout" >&2
  exit 1
fi
grep -Fxq '// user work' "${temporary}/control-plane/gen/proto/new.pb.go"

# Baseline validation precedes every external checker, so these failures need
# no contract tools and cannot quietly fall back to comparing HEAD with itself.
cp "${ROOT}/scripts/check-breaking.sh" "${ROOT}/scripts/env.sh" "${temporary}/scripts/"
for baseline in HEAD does-not-exist; do
  if "${temporary}/scripts/check-breaking.sh" "${baseline}" >"${temporary}/baseline.log" 2>&1; then
    echo "contract check accepted invalid baseline ${baseline}" >&2
    exit 1
  fi
  grep -q 'contract comparison baseline' "${temporary}/baseline.log"
done
