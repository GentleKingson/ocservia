# Vendored iroh provenance

This directory is the unpacked crates.io `iroh` version `1.2.0` release with a
small, repository-local relay-lifecycle patch.

- Registry archive: `iroh-1.2.0.crate`
- crates.io archive SHA-256: `b2f8d1cfffc83efe39a1031aab423ce09cb8048071baba550508931e9a81ce46`
- Upstream repository: `https://github.com/n0-computer/iroh`
- Upstream VCS commit recorded by the archive:
  `17c0612f80f78f5288e97b818b1360ae6ea0a51a`
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
- `src/socket/remote_map.rs`
- `src/socket/remote_map/remote_state.rs`
- `src/socket/transports.rs`
- `src/socket/transports/relay.rs`
- `src/socket/transports/relay/actor.rs`
- `src/test_utils.rs`

The registry extraction marker `.cargo-ok` is intentionally omitted. No other
archive source or manifest is modified.

Local changes add an opt-in `Endpoint` builder setting that keeps all
configured relay connections active, reconciles dynamic relay-map additions
and removals, bounds graceful relay-client close, and fails the home relay over
to an already-connected standby. Concurrent per-relay status publishers update
one mutex-serialized authoritative map before publishing immutable snapshots,
so a healthy relay cannot disappear through a lost read-modify-write. Path
selection excludes disconnected relay paths, reacts immediately to relay-state
changes, and never blocks global
datagram routing on a full queue belonging to an unconnected relay. A
`test-utils`-gated builder hook shortens the non-home idle timeout for
production-graph lifecycle regressions; the production default remains 60
seconds. Persistent connections are disabled by default. Ocservia transportd
and Agent endpoints enable them only for custom relay maps containing at least
two members. Default, disabled, and single-member custom relay modes do not opt
in to persistent connections. Shared connection-state, queue and path-selection
changes are not all gated by that option.

The lifecycle customization originates in ocservia PRs #80/#81 and is rebased
from the 1.0.0 vendor at commit `be808c9f7f4f01e513a2a4e05f6ddeaa497b1805`.
The 1.2.0 release absorbs upstream oversized-batch progress (`2b9f4418`),
random mapped addresses (`a05c9c41`), priority replies during reconnect backoff
(`04583191`), and cancelled-task shutdown (`09a0aca1`). The local per-relay
status map, bounded persistent retry, connected-path selection, and standby
promotion remain: upstream does not provide equivalent opt-in semantics.

The receive loop additionally stops the current batch and schedules another
poll when dropping oversized input. Upstream 1.2.0's `continue` can leave holes
in the returned receive slots; returning Pending after a drop can also strand
already queued traffic without a wake. A `test-utils` bridge exercises this
actual boundary with oversized batched/unbatched inputs and following healthy
packets under the workspace lock. It does not open network connections.

To replay provenance, download and verify the archive above, extract it, and
overlay the eight listed Rust files from the chosen ocservia revision. Add this
provenance file and the two exact upstream license texts. All remaining files
must compare byte-for-byte with the archive. The manifests and vendor lock stay
upstream originals; `rust/Cargo.lock`, not this archive lock, owns production.

The complete upstream test and example suite is not added to the ocservia
workspace. Production relay lifecycle regressions run through the transportd
and Agent crates using `rust/Cargo.lock`, including
`malformed_relay_batches_preserve_receive_progress`. Focused actor/path tests instead use
the preserved archive lockfile at `vendor/iroh/Cargo.lock`; this is a different
dependency graph, not production-graph evidence. From `rust/`, run each focused
test with:

```sh
cargo test --locked --manifest-path vendor/iroh/Cargo.toml \
  --no-default-features --features metrics,tls-ring --lib <test_name>
```

The focused names are `concurrent_relay_status_transitions_preserve_every_actor`,
`datagrams_for_disconnected_relays_are_dropped_not_queued`,
`relay_paths_offered_only_for_connected_relays`,
`home_relay_failover_only_picks_connected_configured_standbys`, and
`persistent_relay_retry_sleep_stays_inside_recovery_budget`,
`test_prio_inbox_answered_during_backoff`, and
`close_all_active_relays_survives_a_cancelled_task`. These tests are not
currently invoked by `scripts/rust-check.sh`; do not infer their execution from
a green workspace-only Rust job.
