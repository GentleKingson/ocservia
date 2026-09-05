#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "$ROOT/scripts/env.sh"
export TMPDIR="${TMPDIR:-/var/tmp}" CARGO_BUILD_JOBS="${CARGO_BUILD_JOBS:-2}"
OUT="${RELAY_RECOVERY_BUILD:?set a private build directory}"
mkdir -p "$OUT/bin" "$OUT/deploy/g6-readiness"
(cd "$ROOT/control-plane" && go build -trimpath -o "$OUT/bin/ocserv-control" ./cmd/ocserv-control)
(cd "$ROOT/rust" && cargo build --locked -p ocservia-transportd --features relay-recovery-test)
cp "$ROOT/rust/target/debug/ocservia-transportd" "$OUT/bin/"
(cd "$ROOT/rust" && cargo build --locked -p ocservia-agent -p ocservia-privd -p ocservia-g6-probe)
for binary in ocservia-agent ocservia-privd ocservia-g6-probe; do
  cp "$ROOT/rust/target/debug/$binary" "$OUT/bin/"
done
cargo install --locked --version 1.0.3 --features server iroh-relay --debug --root "$OUT/relay"
cp "$OUT/relay/bin/iroh-relay" "$OUT/bin/"
cp "$ROOT/deploy/prepare-transport-runtime.sh" "$OUT/deploy/"
cp -R "$ROOT/deploy/production" "$OUT/deploy/"
cp -R "$ROOT/deploy/g6-readiness/fake-ocserv" "$OUT/deploy/g6-readiness/"
cp "$ROOT/deploy/g6-readiness/agent-supervisor.sh" "$OUT/deploy/g6-readiness/"
for stage in control transportd probe relay agent; do
  docker build -f "$ROOT/deploy/g6-readiness/relay-recovery.Dockerfile" \
    --target "$stage" -t "ocservia-relay-recovery:$stage" "$OUT"
done
sha256sum "$OUT/bin/"* > "$OUT/binaries.sha256"
