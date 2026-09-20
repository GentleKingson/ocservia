# Standalone Relay Build

`bash scripts/build-relay.sh <install-root> [release|debug]` builds the unmodified
crates.io `iroh-relay` CLI. `toolchains.lock` selects the Relay version, while
`scripts/checksums.txt` pins the archive before extraction. Production Docker
and the isolated recovery builder share this entry point.

For 1.2.0, the archive SHA-256 is
`beb2294a9749d6a25fd7cd8bcf0fccd932f20716d4f967d85d3f23c7135ae6a2`.
`relay.Cargo.lock` starts from that archive's lock and applies only
`cargo update -p rustls --precise 0.23.45`, including its required aws-lc and
webpki dependencies. This fixes GHSA-2mjx-qc3c-rqvc in the independent binary;
the endpoint workspace lock cannot constrain `cargo install`.

On a future version change, verify the new registry archive checksum, extract
it into a private directory, update only required security dependencies, and
retain the resulting lock here. Build with `--locked`, audit both lockfiles,
and check this extracted package against `rust/deny.toml` for licenses and
sources. The scheduled security workflow audits this lock separately. Do not
replace it with the upstream lock without reviewing its security baseline.

The CLI and configuration remain upstream, licensed `MIT OR Apache-2.0`; no
Relay source fork or authentication bypass is introduced. Endpoint vendoring
and patch provenance are documented in `rust/vendor/iroh/OCSERVIA-PROVENANCE.md`.
