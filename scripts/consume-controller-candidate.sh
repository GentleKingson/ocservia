#!/usr/bin/env bash
# A clean native consumer: verify producer identity before any Registry pull.
set -euo pipefail
: "${CANDIDATE_BUNDLE:?}" "${CANDIDATE_BUNDLE_SHA256:?}" "${CANDIDATE_KEY_SHA256:?}"
: "${CANDIDATE_SHA:?}" "${VERSION:?}" "${CONTROLLER_ARCH:?}" "${ARTIFACT_DIR:?}"
case "${CONTROLLER_ARCH}:$(uname -m)" in amd64:x86_64|arm64:aarch64) ;; *) exit 2 ;; esac
cd "$CANDIDATE_BUNDLE"
for file in candidate-signing.pub.pem SHA256SUMS SHA256SUMS.sig "controller-release-${CONTROLLER_ARCH}.json" registry-bindings.jsonl; do
  [[ -f "$file" && ! -L "$file" ]]
done
[[ "$(sha256sum candidate-signing.pub.pem | cut -d' ' -f1)" == "$CANDIDATE_KEY_SHA256" ]]
[[ "$(sha256sum SHA256SUMS | cut -d' ' -f1)" == "$CANDIDATE_BUNDLE_SHA256" ]]
openssl pkeyutl -verify -rawin -pubin -inkey candidate-signing.pub.pem -in SHA256SUMS -sigfile SHA256SUMS.sig
sha256sum --strict -c SHA256SUMS
manifest="controller-release-${CONTROLLER_ARCH}.json"
jq -e --arg sha "$CANDIDATE_SHA" --arg version "$VERSION" --arg platform "linux/$CONTROLLER_ARCH" '
  .manifest_version == 2 and .source_commit == $sha and .release_version == $version and
  .platform == $platform and (.images | length == 11) and
  all(.images[]; test("^[a-zA-Z0-9._:/-]+@sha256:[0-9a-f]{64}$"))' "$manifest" >/dev/null
while IFS=$'\t' read -r name reference; do
  docker pull --platform "linux/$CONTROLLER_ARCH" "$reference"
  [[ "$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$reference")" == "linux/$CONTROLLER_ARCH" ]]
  config="$(docker image inspect --format '{{.Id}}' "$reference")"
  if [[ "$name" != postgres && "$name" != otel ]]; then
    [[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$reference")" == "$CANDIDATE_SHA" ]]
    jq -se --arg name "$name" --arg platform "linux/$CONTROLLER_ARCH" --arg ref "$reference" --arg config "$config" '
      [.[] | select(.name == $name and .platform == $platform)] |
      length == 1 and .[0].reference == $ref and .[0].config_digest == $config' registry-bindings.jsonl >/dev/null
  fi
  jq -nc --arg candidate_sha "$CANDIDATE_SHA" --arg reference "$reference" --arg name "$name" \
    --arg image_id "$config" --arg platform "linux/$CONTROLLER_ARCH" \
    '{candidate_sha:$candidate_sha,reference:$reference,name:$name,image_id:$image_id,platform:$platform,pulled:true}' \
    >>"$ARTIFACT_DIR/registry-pulls.jsonl"
done < <(jq -r '.images | to_entries[] | [.key,.value] | @tsv' "$manifest")
cp "$manifest" "$ARTIFACT_DIR/candidate-manifest.json"
