#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
export CANDIDATE_BUNDLE="$work/bundle" ARTIFACT_DIR="$work/evidence" CI_TRACE="$work/trace"
export CANDIDATE_SHA VERSION=9.9.9 CONTROLLER_ARCH CONFIG_DIGEST
CANDIDATE_SHA="$(printf 'a%.0s' {1..40})"
CONFIG_DIGEST="sha256:$(printf 'c%.0s' {1..64})"
case "$(uname -m)" in x86_64) CONTROLLER_ARCH=amd64 ;; aarch64) CONTROLLER_ARCH=arm64 ;; *) exit 2 ;; esac
mkdir "$CANDIDATE_BUNDLE" "$ARTIFACT_DIR" "$work/bin"
args=()
for name in gateway control transport backup edge relay signer mysql_backup mariadb_backup postgres otel; do
  reference="ghcr.io/gentlekingson/ocservia/${name}@${CONFIG_DIGEST}"
  args+=(--image "$name=$reference")
  if [[ "$name" != postgres && "$name" != otel ]]; then
    jq -nc --arg name "$name" --arg ref "$reference" --arg config "$CONFIG_DIGEST" --arg platform "linux/$CONTROLLER_ARCH" \
      '{name:$name,reference:$ref,config_digest:$config,platform:$platform}' >>"$CANDIDATE_BUNDLE/registry-bindings.jsonl"
  fi
done
manifest="$CANDIDATE_BUNDLE/controller-release-${CONTROLLER_ARCH}.json"
node "$ROOT/scripts/generate-controller-release-manifest.mjs" --manifest-version 2 --output "$manifest" \
  --platform "linux/$CONTROLLER_ARCH" --release-version "$VERSION" --release-tag "v$VERSION" \
  --source-commit "$CANDIDATE_SHA" --migration-dir "$ROOT/control-plane/migrations" "${args[@]}"
openssl genpkey -algorithm ED25519 -out "$work/key" >/dev/null 2>&1
openssl pkey -in "$work/key" -pubout -out "$CANDIDATE_BUNDLE/candidate-signing.pub.pem"
(cd "$CANDIDATE_BUNDLE" && sha256sum "controller-release-${CONTROLLER_ARCH}.json" registry-bindings.jsonl >SHA256SUMS)
openssl pkeyutl -sign -rawin -inkey "$work/key" -in "$CANDIDATE_BUNDLE/SHA256SUMS" -out "$CANDIDATE_BUNDLE/SHA256SUMS.sig"
export CANDIDATE_KEY_SHA256 CANDIDATE_BUNDLE_SHA256
CANDIDATE_KEY_SHA256="$(sha256sum "$CANDIDATE_BUNDLE/candidate-signing.pub.pem" | cut -d' ' -f1)"
CANDIDATE_BUNDLE_SHA256="$(sha256sum "$CANDIDATE_BUNDLE/SHA256SUMS" | cut -d' ' -f1)"
cat >"$work/bin/docker" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == pull ]]; then echo pull >>"$CI_TRACE"; exit 0; fi
[[ "$1 $2 $3" == 'image inspect --format' ]]
case "$4" in
  '{{.Os}}/{{.Architecture}}') echo "linux/$CONTROLLER_ARCH" ;;
  '{{.Id}}') echo "$CONFIG_DIGEST" ;;
  '{{index .Config.Labels "org.opencontainers.image.revision"}}') echo "$CANDIDATE_SHA" ;;
  *) exit 2 ;;
esac
STUB
chmod 755 "$work/bin/docker"
export PATH="$work/bin:$PATH"
bash "$ROOT/scripts/consume-controller-candidate.sh" >/dev/null
[[ "$(wc -l <"$CI_TRACE")" == 11 ]]
: >"$CI_TRACE"
if CANDIDATE_KEY_SHA256=bad bash "$ROOT/scripts/consume-controller-candidate.sh" >/dev/null 2>&1; then exit 1; fi
if CANDIDATE_BUNDLE_SHA256=bad bash "$ROOT/scripts/consume-controller-candidate.sh" >/dev/null 2>&1; then exit 1; fi
if CANDIDATE_SHA="$(printf 'b%.0s' {1..40})" bash "$ROOT/scripts/consume-controller-candidate.sh" >/dev/null 2>&1; then exit 1; fi
printf '\n' >>"$manifest"
if bash "$ROOT/scripts/consume-controller-candidate.sh" >/dev/null 2>&1; then exit 1; fi
[[ ! -s "$CI_TRACE" ]]
echo 'Candidate identity, trusted producer hashes and signature checked before pulls'
