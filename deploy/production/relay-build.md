# Standalone Relay build

`bash scripts/build-relay.sh <install-root> [release|debug]` builds the unmodified
crates.io `iroh-relay` CLI. `toolchains.lock` selects the Relay version, while
`scripts/checksums.txt` pins the archive before extraction. "Standalone" here
means the stand-alone `iroh-relay` binary, not the Standalone deployment mode.
The production Relay image build (`deploy/production/relay.Dockerfile`, which the
Integrated Relay service reuses) uses this entry point. It builds the Relay binary
only; exactly one Relay is supported per deployment, see
[dedicated Relay](../../docs/how-to/dedicated-relay.md).

For 1.4.0, the archive SHA-256 is
`99f1da3af46c3bca658076e47fea877f38108dae664507a1fb35d276314363c8`.
`relay.Cargo.lock` starts from the 1.4.0 archive lock. It already selects
`rustls` 0.23.46, retaining the previous production security baseline for
RUSTSEC-2026-0346, RUSTSEC-2026-0347, RUSTSEC-2026-0348 and
RUSTSEC-2026-0349. The endpoint workspace lock cannot constrain `cargo install`.

Both production locks additionally update `rustls-webpki` to 0.103.16,
`webpki-roots` to 1.0.10 and `webpki-root-certs` to 1.0.10 for
RUSTSEC-2026-0382, RUSTSEC-2026-0381 and RUSTSEC-2026-0383. These
certificate distrust updates were published after the upstream archive.
Both production locks also select `tokio` 1.53.3 for RUSTSEC-2026-0386.
The vendor test lock remains the original archive graph and is not used for
production builds.

On a future version change, verify the new registry archive checksum, extract
it into a private directory, update only required security dependencies, and
retain the resulting lock here. Build with `--locked`, audit both lockfiles,
and check this extracted package against `rust/deny.toml` for licenses and
sources. The scheduled security workflow audits this lock separately. Do not
replace it with the upstream lock without reviewing its security baseline.

The CLI and configuration remain upstream, licensed `MIT OR Apache-2.0`; no
Relay source fork or authentication bypass is introduced. Endpoint vendoring
and patch provenance are documented in `rust/vendor/iroh/OCSERVIA-PROVENANCE.md`.
