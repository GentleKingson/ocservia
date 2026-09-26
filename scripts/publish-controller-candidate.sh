#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${VERSION:?}" "${GITHUB_SHA:?}" "${GITHUB_RUN_ID:?}" "${GITHUB_RUN_ATTEMPT:?}"
: "${IMAGES_DIR:?}" "${SECURITY_SUMMARY:?}" "${OUTPUT_DIR:?}"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ && "$GITHUB_SHA" =~ ^[0-9a-f]{40}$ ]]
[[ "$(git -C "$ROOT" rev-parse HEAD)" == "$GITHUB_SHA" ]]
prefix="ghcr.io/${GITHUB_REPOSITORY,,}"
tag="candidate-${GITHUB_SHA}-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
mkdir -p "$OUTPUT_DIR"
jq -e --arg sha "$GITHUB_SHA" '.source_commit == $sha and (.images | length == 9) and
  all(.images[].platforms[]; .gate == "pass")' "$SECURITY_SUMMARY" >/dev/null
cp "$SECURITY_SUMMARY" "$OUTPUT_DIR/controller-image-security.json"
jq -n --arg sha "$GITHUB_SHA" --arg version "$VERSION" --arg run "$GITHUB_RUN_ID" --arg attempt "$GITHUB_RUN_ATTEMPT" \
  --argjson products "${CANDIDATE_PRODUCTS_JSON:?}" \
  '{source_commit:$sha,version:$version,run_id:$run,run_attempt:$attempt,products:$products}' >"$OUTPUT_DIR/producer.json"
: >"$OUTPUT_DIR/registry-bindings.jsonl"
for arch in amd64 arm64; do
  args=()
  for name in gateway control transport backup edge relay signer mysql_backup mariadb_backup; do
    source="${prefix}/${name}:${VERSION}-linux-${arch}"
    target="${prefix}/${name}:${tag}-${arch}"
    docker load --input "$IMAGES_DIR/${name}-linux-${arch}.tar" >/dev/null
    config="$(docker image inspect --format '{{.Id}}' "$source")"
    [[ "$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$source")" == "linux/${arch}" ]]
    [[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$source")" == "$GITHUB_SHA" ]]
    [[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$source")" == "$VERSION" ]]
    [[ "$config" == "$(jq -er --arg name "$name" --arg arch "linux-${arch}" '.images[$name].platforms[$arch].config_digest' "$SECURITY_SUMMARY")" ]]
    docker tag "$source" "$target"
    docker push "$target"
    digest="$(docker buildx imagetools inspect --format '{{.Manifest.Digest}}' "$target")"
    [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]
    reference="${prefix}/${name}@${digest}"
    args+=(--image "${name}=${reference}")
    jq -nc --arg sha "$GITHUB_SHA" --arg name "$name" --arg arch "$arch" \
      --arg reference "$reference" --arg config "$config" \
      '{source_commit:$sha,name:$name,platform:("linux/"+$arch),reference:$reference,config_digest:$config}' \
      >>"$OUTPUT_DIR/registry-bindings.jsonl"
    docker image rm "$target" "$source" >/dev/null
  done
  node "$ROOT/scripts/generate-controller-release-manifest.mjs" --manifest-version 2 \
    --output "$OUTPUT_DIR/controller-release-${arch}.json" --platform "linux/${arch}" \
    --release-version "$VERSION" --release-tag "v${VERSION}" --source-commit "$GITHUB_SHA" \
    --migration-dir "$ROOT/control-plane/migrations" "${args[@]}" \
    --image postgres=docker.io/library/postgres@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f \
    --image otel=docker.io/otel/opentelemetry-collector@sha256:0c066d4388070dad8dc9961d9f23649e85a226620e6b359334e4a6c7f9d73b23
done
key="$(mktemp)"
trap 'rm -f -- "$key"' EXIT
openssl genpkey -algorithm ED25519 -out "$key"
openssl pkey -in "$key" -pubout -out "$OUTPUT_DIR/candidate-signing.pub.pem"
cd "$OUTPUT_DIR"
sha256sum controller-release-*.json registry-bindings.jsonl controller-image-security.json producer.json >SHA256SUMS
openssl pkeyutl -sign -rawin -inkey "$key" -in SHA256SUMS -out SHA256SUMS.sig
for arch in amd64 arm64; do
  sha256sum "controller-release-${arch}.json" >"controller-release-${arch}.json.sha256"
done
echo "key-sha256=$(sha256sum candidate-signing.pub.pem | cut -d' ' -f1)" >>"${GITHUB_OUTPUT:?}"
echo "sha256=$(sha256sum SHA256SUMS | cut -d' ' -f1)" >>"$GITHUB_OUTPUT"
