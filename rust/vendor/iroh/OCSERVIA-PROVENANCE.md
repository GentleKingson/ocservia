# Vendored iroh provenance

This directory is the unpacked crates.io `iroh` version `1.0.0` release with a
small, repository-local relay-lifecycle patch.

- Registry archive: `iroh-1.0.0.crate`
- crates.io archive SHA-256: `6435544bb3a5c4e6ff7affaa0c0aa0d1bca45bd700226329d5059d3eb54f9dff`
- Upstream repository: `https://github.com/n0-computer/iroh`
- Upstream VCS commit recorded by the archive:
  `6520cd6c9bacea15823c53a38267c40d754980fe`
- Declared upstream license: `MIT OR Apache-2.0`
- Upstream `LICENSE-MIT` SHA-256:
  `f169adb8124d3b005416d8485d00777c9a7bdd9099982c52a4493f9732e6d050`
- Upstream `LICENSE-APACHE` SHA-256:
  `903131e2786f073a942fbf8fae122d9e576e4dad758c6da7f9f2ba58fd8611ab`

The crates.io archive does not contain separate MIT or Apache license text
files. Exact `LICENSE-MIT` and `LICENSE-APACHE` files from the upstream VCS
commit above are included for redistribution. The archive's original
`LICENSE-BSD3`, package manifests, README, and `.cargo_vcs_info.json` are also
preserved. The license expression above is copied from the archive's
normalized and original Cargo manifests and checked by the workspace's
`cargo deny` policy.

The repository-local patch changes only these upstream Rust sources:

- `src/endpoint.rs`
- `src/socket.rs`
- `src/socket/transports.rs`
- `src/socket/transports/relay.rs`
- `src/socket/transports/relay/actor.rs`

The registry extraction marker `.cargo-ok` is intentionally omitted. No other
archive source or manifest is modified.

Local changes add an opt-in `Endpoint` builder setting that keeps all
configured relay connections active, reconciles dynamic relay-map additions
and removals, and bounds graceful relay-client close. A `test-utils`-gated
builder hook shortens the non-home idle timeout for production-graph lifecycle
regressions; the production default remains 60 seconds. Persistent connections
are disabled by default. Ocservia transportd and Agent endpoints enable them
only for custom relay maps containing at least two members. Default, disabled,
and single-member custom relay modes retain the upstream behavior. The complete
upstream test and example suite is not added to the ocservia workspace. Relay
lifecycle regressions execute through the production crates against the
workspace lock graph.
