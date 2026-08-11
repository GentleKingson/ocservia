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
  # Enrollment tokens are intentionally endpoint-bound. Normalize only this
  # security migration in the comparison baseline so every unrelated OpenAPI
  # break remains an error.
  (cd "${ROOT}/web" && node --input-type=module - \
    "${temporary}/main.yaml" "${ROOT}/openapi/openapi.yaml" <<'EOF'
import { readFileSync, writeFileSync } from "node:fs";
import { parse, stringify } from "yaml";

const [, , baselinePath, candidatePath] = process.argv;
const baseline = parse(readFileSync(baselinePath, "utf8"));
const candidate = parse(readFileSync(candidatePath, "utf8"));
const name = "EnrollmentTokenRequest";
const field = "expected_endpoint_id";
const candidateSchema = candidate.components?.schemas?.[name];
if (!candidateSchema?.required?.includes(field)) {
  throw new Error(`${name}.${field} must remain required`);
}
const baselineSchema = baseline.components?.schemas?.[name];
if (!baselineSchema?.properties?.[field]) {
  throw new Error(`${name}.${field} is missing from the comparison baseline`);
}
baselineSchema.required = [...new Set([...(baselineSchema.required ?? []), field])];
writeFileSync(baselinePath, stringify(baseline));
EOF
  )
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
