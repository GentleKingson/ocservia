#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

if git -C "${ROOT}" cat-file -e origin/main:proto/buf.yaml 2>/dev/null; then
  (cd "${ROOT}" && buf breaking proto --against '.git#branch=origin/main,subdir=proto')
else
  echo "No contract exists on origin/main; validating FILE policy with a breaking fixture."
  temporary="$(mktemp -d)"
  trap 'rm -rf "${temporary}"' EXIT INT TERM
  cp -R "${ROOT}/proto" "${temporary}/against"
  cp -R "${ROOT}/proto" "${temporary}/candidate"
  sed -i.bak '/string version = 2;/d' \
    "${temporary}/candidate/ocserv/platform/transport/v1/transport.proto"
  rm -f "${temporary}/candidate/ocserv/platform/transport/v1/transport.proto.bak"
  if (cd "${temporary}" && buf breaking candidate --against against); then
    echo "breaking fixture was not rejected" >&2
    exit 1
  fi
fi

if git -C "${ROOT}" cat-file -e origin/main:openapi/openapi.yaml 2>/dev/null; then
  temporary="$(mktemp -d)"
  trap 'rm -rf "${temporary}"' EXIT INT TERM
  git -C "${ROOT}" show origin/main:openapi/openapi.yaml >"${temporary}/main.yaml"
  oasdiff breaking --fail-on ERR \
    "${temporary}/main.yaml" "${ROOT}/openapi/openapi.yaml"
else
  echo "No OpenAPI contract exists on origin/main; validating compatibility detection with a fixture."
  temporary="$(mktemp -d)"
  trap 'rm -rf "${temporary}"' EXIT INT TERM
  (cd "${ROOT}/web" && node --input-type=module - \
    "${ROOT}/openapi/openapi.yaml" "${temporary}/candidate.yaml" <<'EOF'
import { readFileSync, writeFileSync } from "node:fs";
import { parse, stringify } from "yaml";

const [, , source, destination] = process.argv;
const document = parse(readFileSync(source, "utf8"));
delete document.paths["/operations"];
writeFileSync(destination, stringify(document));
EOF
  )
  if oasdiff breaking --fail-on ERR \
    "${ROOT}/openapi/openapi.yaml" "${temporary}/candidate.yaml"; then
    echo "OpenAPI breaking fixture was not rejected" >&2
    exit 1
  fi
fi
