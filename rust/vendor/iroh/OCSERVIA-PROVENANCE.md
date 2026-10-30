# Vendored iroh provenance

This directory is the unpacked crates.io `iroh` version `1.4.0` release with a
small, repository-local relay-lifecycle patch.

- Registry archive: `iroh-1.4.0.crate`
- crates.io archive SHA-256: `a11e776f1bf36f140e465133ff4108e46bd37a3d10ecdf5b1ea8dfb3bd35a7d2`
- Upstream repository: `https://github.com/n0-computer/iroh`
- Upstream VCS commit recorded by the archive:
  `55908a5809b61c72ec8deeff024038f4a9d68ab7`
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
- `src/socket/mapped_addrs.rs`
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
changes, and never blocks global datagram routing on a full queue belonging to
an unconnected relay. A `test-utils`-gated builder hook shortens the non-home idle timeout for
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

The receive loop is now unmodified upstream 1.4.0: PR #4573
(`26dfe1d469e274d38fe3b41617a20f9bb7a10072`) indexes outputs by delivered
messages and wakes after exhausting a batch of drops. The previous local
break-and-wake implementation is removed. The existing `test-utils` bridge
still exercises oversized batched/unbatched input followed by healthy packets
under the production workspace lock, without opening network connections.

## Local patch decisions for 1.4.0

| Source | Decision and evidence |
| --- | --- |
| `src/endpoint.rs` | Retain builder fields, `keep_relays_connected` and test-only idle timeout; upstream has no equivalent setting. Preserve upstream bind tracing and additive datagram APIs. |
| `src/socket.rs` | Retain option/status wiring and async relay-map reconciliation; preserve upstream immediate local-address updates and proxy storage. |
| `src/socket/remote_map.rs` | Retain status watcher plumbing; keep its publisher alive in the shared test fixture so upstream Initial-send regressions exercise a live actor. |
| `src/socket/remote_map/remote_state.rs` | Retain disconnected-relay path filtering and status-triggered reselection. Upstream #4512 bounds Initial send tasks and #4551 deduplicates pending paths; neither supplies connection-state filtering. Preserve both upstream fixes and their tests. |
| `src/socket/transports.rs` | Retain combined status watcher, relay-map notifications and production-graph receive-test bridge; preserve upstream datagram send refactor. |
| `src/socket/transports/relay.rs` | Use upstream #4573 receive implementation; retain status wiring, map notification and existing regression bridge. |
| `src/socket/transports/relay/actor.rs` | Retain serialized status publication, disconnected queue handling, opt-in persistence, retry/close bounds, reconciliation and standby promotion. Upstream only changes connectivity log levels here; preserve those levels. |
| `src/test_utils.rs` | Retain production-lock receive regression entrypoint; upstream unit tests alone use a different graph. |

The status/path/queue changes also apply when persistence is disabled, including
single-Relay configurations. None is removed on the assumption that production
supports only one Relay. The seven existing focused regression names remain
stable; no new product support or discovery service is enabled.

Relay receive mappings are capped at 65,536 entries before unauthenticated
sender metadata reaches QUIC. On pressure, mappings without a remote actor are
reclaimed in both directions; actor-owned mappings retain their addresses.
New receive mappings are refused if the retained entries fill the cap. Queued
incoming handshakes without an actor may need to retry under this pressure;
established and locally initiated actor-owned paths are preserved.

Endpoint-identity mappings are capped at 65,536 entries. A new remote is refused
when the cap is full of mappings that have a running remote actor; otherwise
mappings without an actor are reclaimed first. A remote actor that exits with
no connection, queued message or resolve request also releases its mapping in
both directions. Each Relay connection keeps at most 4,096 sender hints: a hint
is recorded only after the datagram is queued downstream, the smallest id is
evicted when full without relying on `EndpointGone`, and `EndpointGone` also
clears the last-sender shortcut so an evicted live peer registers again on its
next datagram. Both receive loops use the same handler.

To replay provenance, download and verify the archive above, extract it, and
overlay the nine listed Rust files from the chosen ocservia revision. Add this
provenance file and the two exact upstream license texts. All remaining files
must compare byte-for-byte with the archive. The manifests and vendor lock stay
upstream originals; `rust/Cargo.lock`, not this archive lock, owns production.

The complete upstream test and example suite is not added to the ocservia
workspace. Production relay lifecycle regressions run through the transportd
and Agent crates using `rust/Cargo.lock`, including
`malformed_relay_batches_preserve_receive_progress`. Focused actor/path tests
instead use the preserved archive lockfile at `vendor/iroh/Cargo.lock`; this is a different
dependency graph, not production-graph evidence. Its preserved archive versions
of `rustls-webpki` (0.103.15), `webpki-roots` and `webpki-root-certs` (1.0.9)
precede the certificate distrust updates in both production locks (0.103.16
and 1.0.10 respectively). Do not use the vendor test graph for deployment.
From `rust/`, run each focused test with:

```sh
cargo test --locked --manifest-path vendor/iroh/Cargo.toml \
  --no-default-features --features metrics,tls-ring --lib <test_name>
```

The focused names are `concurrent_relay_status_transitions_preserve_every_actor`,
`datagrams_for_disconnected_relays_are_dropped_not_queued`,
`relay_paths_offered_only_for_connected_relays`,
`home_relay_failover_only_picks_connected_configured_standbys`,
`persistent_relay_retry_sleep_stays_inside_recovery_budget`,
`test_prio_inbox_answered_during_backoff`, and
`close_all_active_relays_survives_a_cancelled_task`. These tests are not
invoked by `scripts/rust-check.sh` or Quick CI; the Full profile of
`.github/workflows/ci.yml` (also run by Release Check) runs all seven by exact
name and fails unless all seven ran. Do not infer their execution from a green
Quick Rust job.
