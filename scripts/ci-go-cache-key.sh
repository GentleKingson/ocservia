#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
cd "${ROOT}"

# Never restore across a toolchain, compiler, dependency or build-policy change.
identity="$(
  set -e
  {
    cat /etc/os-release
    printf '%s\n' "${ImageOS:-}" "${ImageVersion:-}"
    go env GOVERSION GOHOSTOS GOHOSTARCH GOOS GOARCH GOAMD64 GOARM64 GOEXPERIMENT \
      CGO_ENABLED CC CGO_CFLAGS CGO_CPPFLAGS CGO_CXXFLAGS CGO_LDFLAGS GOFLAGS
    if [[ "$(go env CGO_ENABLED)" == 1 ]]; then
      read -r -a compiler <<<"$(go env CC)"
      "${compiler[@]}" --version
    fi
    sha256sum toolchains.lock scripts/checksums.txt scripts/bootstrap.sh scripts/env.sh \
      scripts/ci-go-cache-key.sh scripts/go-check.sh scripts/required-go-tests.sh \
      scripts/database-integration.sh scripts/database-foundation-integration.sh \
      go.work go.work.sum control-plane/go.mod control-plane/go.sum \
      tools/g6-harness/go.*
  } | sha256sum
)"
printf 'identity=%s\n' "${identity%% *}"
