# Native candidate business probe

This manual supplement exercises the signed production Controller lifecycle,
real Local logins, a native systemd Agent/privd/ocserv node and an OpenConnect
network namespace. It reuses the existing release builds, single-Relay private
CA configuration and receipt provisioning. It is not a new deployment platform,
a simulator, the T06 upgrade gate, or complete T07 acceptance.

With explicit authorization to use disposable GitHub-hosted runners, dispatch
`release-upgrade.yml` on the exact candidate branch with `version`,
`candidate_sha`, `baseline_release=v0.6.2` and `business_only=true`. The baseline
input is unused in this mode; the ordinary native upgrade remains the default.
This mode does not publish packages/images, create a tag, or modify Secrets.
Local preparation checks must run in an isolated checkout on `BuildServer`.
Never run the business driver or its host-installing steps on BuildServer.

## Evidence and boundaries

The driver freezes the candidate SHA, builds signed matched native packages and
digest-pinned Controller images, then uses `controller.sh install --release-file`.
The same independently held task signing key verifies the native package before
`dpkg`; the embedded payload verifier remains enabled. Managed-node preparation
and the final `SERVICES_ACTIVE` convergence use the shipped installer.

The following limitations are deliberately retained, not scored as PASS:

- An unpublished candidate cannot exercise the fixed GitHub Release download
  and versioned Controller bootstrap. This publication-dependent evidence is
  deferred to T09/formal release, not a perpetual prepublication T07 blocker.
  Signed candidate lifecycle and managed-node preparation/convergence remain
  T07 requirements. No fabricated tag, download stub, unsigned substitution or
  published-release identity is used.
- The private Relay CA uses the existing explicit binary CA option, a rendered
  production transport descriptor and a test-only systemd launcher copy, as in
  single-Relay validation. Enrollment uses the real CLI with that CA. This is
  strict TLS but not unchanged production launchers or the complete one-command
  managed-node enrollment path.
- Local requester and approver authenticate normally as different identities;
  no database-inserted sessions or devAuth are used. This verifies self-approval
  rejection, not independent custody by two people. Database writes are limited
  to documented initial workspace provisioning before normal Local bootstrap.
- External OIDC, mixed-mode login, external signer/CA, certificate/P12 lifecycle,
  positive configuration plan/apply and browser interaction need their own real
  environment and evidence. A deliberately unreachable signer URL is not a
  working signer and must not receive a certificate success result.
- One host, one architecture and one Relay provide no Relay redundancy or T08
  fault-domain/SLO proof. The client namespace protects host routes; it is not
  another host. The probe checks test VPN traffic, never real user traffic.

`result.json` therefore keeps `t07_status=BLOCKED`, even when the available
probe passes. A green Actions job is not T07 closure. Per-phase checkpoints are
written only after assertions pass. Missing checkpoints are NOT RUN; preserve
failures, original run/attempt and exact SHA across retries.

Retain the artifact and its GitHub digest before expiry, with product digests,
manifest, environment inventory, timestamps, exit status and API/DB/journal/root
receipt evidence. Logs are private until redacted. Never upload the workspace
secret directory, cookies, raw database or password file. Cleanup removes only
task containers, network namespace, builder and private directory; the native
installation lives only on the disposable hosted runner. Do not generalize this
cleanup to a shared host.

To close T07, cover the prepublication production installation paths with the
reviewed signed candidate, independently controlled operators and provisioned
external services; obtain real positive and critical rejection evidence for
every promised workflow. Publication-dependent immutable download paths belong
to T09/formal release. Positive configuration apply remains unavailable until
the complete matched-node configuration/TLS SecretRef contract is reviewed and
accepted; if promised for 1.0, this still blocks T07/readiness. Preserve the
finite compatibility exclusions and matched-release recovery boundary in
[stable contracts](../reference/stable-contracts.md). Do not import earlier T03,
T04, T05 or T06 results as runtime acceptance of the new candidate.

Freeze the final candidate SHA/tree only after product, deployment, contract
and probe edits stop. Keep later evidence outside the repository. Every final
result must name that SHA, never splice artifacts from earlier commits or
attempts. Changes to Compose, the verification-key contract and native units
require a fresh complete `release-upgrade.yml` run from v0.6.2 with
`version=0.7.0`, `candidate_sha` equal to the frozen SHA and
`session_compatibility=false`, `session_only=false`, `business_only=false`.
All four native Agent/Controller architecture jobs and the aggregate must pass
in the same run/attempt. Business success does not replace this gate.

Existing [single-Relay](single-relay-validation.md) and
[cross-VM enrollment](real-e2e.md) profiles retain their original scope.
