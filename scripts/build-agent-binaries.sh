#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${VERSION:?}" "${PACKAGE_ARCH:?}"
[[ "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || exit 2
case "${PACKAGE_ARCH}:$(uname -m)" in
  amd64:x86_64) machine='Advanced Micro Devices X86-64' ;;
  arm64:aarch64) machine=AArch64 ;;
  *) echo 'Agent build requires the matching native Linux architecture' >&2; exit 2 ;;
esac
case "${PACKAGE_ARCH}:$(docker info --format '{{.Architecture}}')" in
  amd64:x86_64|amd64:amd64|arm64:aarch64|arm64:arm64) ;;
  *) echo 'Docker daemon must match the native Agent build architecture' >&2; exit 2 ;;
esac
builder_hash="$(sha256sum "${ROOT}/rust/agent-build.Dockerfile" | cut -c1-16)"
builder="ocservia-agent-build:${PACKAGE_ARCH}-${builder_hash}"
docker build --platform "linux/${PACKAGE_ARCH}" --tag "${builder}" - <"${ROOT}/rust/agent-build.Dockerfile"
[[ "$(docker image inspect --format '{{.Architecture}}' "${builder}")" == "${PACKAGE_ARCH}" ]]
docker run --rm --platform "linux/${PACKAGE_ARCH}" \
  --user "$(id -u):$(id -g)" --cap-drop=ALL --security-opt=no-new-privileges \
  --volume "${ROOT}:${ROOT}" --workdir "${ROOT}" \
  --env VERSION --env PACKAGE_ARCH --env "ELF_MACHINE=${machine}" \
  --env "BUILD_CACHE_KEY=${PACKAGE_ARCH}-${builder_hash}" \
  --env TAR_OPTIONS=--no-same-owner \
  --env CARGO_INCREMENTAL=0 --env "HOME=${ROOT}/.cache/agent-build-home" \
  "${builder}" bash -euo pipefail -c '
    mkdir -p "${HOME}"
    source scripts/env.sh
    bash scripts/bootstrap.sh native-packages
    case "${PACKAGE_ARCH}:$(uname -m)" in amd64:x86_64|arm64:aarch64) ;; *) exit 2 ;; esac
    [[ "$(getconf GNU_LIBC_VERSION)" == "glibc 2.34" ]]
    rpm -q glibc gcc
    rustc --version
    # Never reuse objects compiled against the Ubuntu host libc.
    export CARGO_TARGET_DIR="${OCSERVIA_ROOT}/rust/target/agent-${BUILD_CACHE_KEY}"
    cd rust
    OCSERV_AGENT_RELEASE_VERSION="${VERSION}" cargo build --locked --release \
      --package ocservia-agent --package ocservia-privd --package ocservia-upgrader
    for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
      path="${CARGO_TARGET_DIR}/release/${binary}"
      readelf -h "${path}" | grep -F "${ELF_MACHINE}"
      [[ "$("${path}" --version)" == "${binary} ${VERSION}" ]]
      file "${path}"
      readelf --version-info "${path}"
    done
    mkdir -p target/release
    for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
      install -m 0755 "${CARGO_TARGET_DIR}/release/${binary}" "target/release/${binary}"
    done
  '
