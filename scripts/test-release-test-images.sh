#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf -- "${work}"' EXIT
mkdir -p "${work}/repo/scripts" "${work}/bin"
cp "${ROOT}/scripts/"{release-test-images.sh,release-artifacts.mjs,g6-buildx-cache.sh} "${work}/repo/scripts/"
cat >"${work}/bin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${TRACE}"
case "$1 $2" in
  'buildx build')
    while (( $# )); do
      if [[ "$1" == --output ]]; then
        output="${2#type=local,dest=}"
        printf 'tunnel\n' >"${output}/ocservia-g6-tunnel"
      fi
      shift
    done ;;
  'save -o') printf '%s\n' "$4" >"$3" ;;
  'load -i') test -f "$3" ;;
  'image inspect')
    if [[ "$3" == --format ]]; then printf 'sha256:%064d\n' 1
    else jq -n --arg sha "${FAKE_SHA:-$GITHUB_SHA}" --arg arch "${FAKE_ARCH:-$ARCH}" \
      '[{Architecture:$arch,Config:{Labels:{"org.opencontainers.image.revision":$sha}}}]'; fi ;;
  *) exit 2 ;;
esac
SH
chmod +x "${work}/bin/docker"
cd "${work}/repo"
git init -q
git -c user.name=test -c user.email=test@example.invalid commit --allow-empty -qm fixture
GITHUB_SHA="$(git rev-parse HEAD)"
export GITHUB_SHA VERSION=1.0.1 G6_CACHE_AVAILABLE=false
export PATH="${work}/bin:${PATH}" TRACE="${work}/trace" GITHUB_OUTPUT="${work}/output" GITHUB_ENV="${work}/env"
component=test-helpers
for ARCH in amd64 arm64; do
  export ARCH
  directory="${work}/${ARCH}-${component}"
  : >"${TRACE}"; : >"${GITHUB_OUTPUT}"; : >"${GITHUB_ENV}"
  bash scripts/release-test-images.sh build "${component}" "${ARCH}" "${directory}"
  expected=3
  [[ "$(grep -c '^buildx build ' "${TRACE}")" == "${expected}" ]]
  [[ "$(grep '^buildx build ' "${TRACE}" | grep -c -- "--platform linux/${ARCH}")" == "${expected}" ]]
  TEST_IMAGES_SHA256="$(sed -n 's/^sha256=//p' "${GITHUB_OUTPUT}")"
  export TEST_IMAGES_SHA256
  : >"${TRACE}"
  bash scripts/release-test-images.sh load "${component}" "${ARCH}" "${directory}"
  if grep -q '^buildx build ' "${TRACE}"; then exit 1; fi
  test -s "${GITHUB_ENV}"
  if FAKE_ARCH=wrong bash scripts/release-test-images.sh load "${component}" "${ARCH}" "${directory}"; then exit 1; fi
  if FAKE_SHA=wrong bash scripts/release-test-images.sh load "${component}" "${ARCH}" "${directory}"; then exit 1; fi
  file="$(find "${directory}" -name '*.tar' -print -quit)"
  printf 'tampered\n' >>"${file}"
  : >"${TRACE}"
  if bash scripts/release-test-images.sh load "${component}" "${ARCH}" "${directory}" 2>/dev/null; then exit 1; fi
  test ! -s "${TRACE}"
done
echo 'Native fixture recipes, build-free loading and identity/tamper rejection passed (mock Docker)'
