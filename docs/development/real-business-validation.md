# Native candidate business probe

This manual supplement exercises the signed production Controller lifecycle,
real Local logins, a native systemd Agent/privd/ocserv node and an OpenConnect
network namespace. It reuses the existing release builds, single-Relay private
CA configuration and receipt provisioning. It is not a new deployment platform,
a simulator, the T06 upgrade gate, or complete T07 acceptance.

With explicit authorization to use disposable GitHub-hosted runners, dispatch
`release-upgrade.yml` on the exact candidate branch with `version`,
`candidate_sha`, `baseline_release=v1.0.0` and `business_only=true`. The baseline
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
- The private Relay CA is provisioned through the documented protected public
  CA files. The shipped Compose overlay and native launcher use the existing
  strict-TLS binary option without descriptor/launcher edits. Enrollment and
  its final node configuration are produced by the managed-node installer;
  preparation, PENDING_APPROVAL and SERVICES_ACTIVE checkpoints remain separate.
  This does not turn the preinstalled signed candidate into a published Release
  download test.
- Local requester and approver authenticate normally as different principals
  with separate credentials and isolated sessions; no database-inserted sessions
  or devAuth are used. Verify RBAC, workspace authorization, self-approval 403,
  approval content binding and replay limits, not merely different identity IDs.
  This does not prove independent custody by two people. Database writes are limited
  to documented initial workspace provisioning before normal Local bootstrap.
- The dedicated HTTPS OIDC fixture exercises discovery, PKCE, callback and
  issuer/signature/nonce/code/state rejection in mixed mode, plus OIDC-only
  login, workspace authorization and restart/logout behavior. The signer uses
  actual OpenSSL CSR verification, signing, public-key sealing and revocation
  acknowledgement over authenticated TLS. These are task-controlled external
  service fixtures, not production IdP/CA/HSM certification or independent
  operator custody. The Controller container trust store is provisioned with
  the task CA; TLS verification stays enabled.
- Native certificate/P12 checks include node restarts and one-use download.
  The real browser uses the signed gateway and Controller API without route
  mocks, simulator or TLS exceptions. Missing phase checkpoints remain NOT RUN;
  source implementation is not proof of runtime success. The reviewed complete
  node-local TLS profile is exercised through browser plan/approval/apply and
  native exact-byte rollback/restart checks; only their checkpoints prove that
  a particular candidate passed.
- One host, one architecture and one Relay provide no Relay redundancy or T08
  fault-domain/SLO proof. The client namespace protects host routes; it is not
  another host. The probe checks test VPN traffic, never real user traffic.

`result.json` therefore keeps `t07_status=NOT_EVALUATED` and explicitly records
`operator_mode=simulated_two_principals` and human custody `NOT_VERIFIED`, even
when the probe passes. Baseline 1.0 and T07 require independently controlled
requester and approver principals, not an organizational two-person rule.
Automated acceptance may authenticate and exercise both principals separately;
it must preserve the RBAC, binding, self-approval and replay checks above.
Actual two-person credential custody is EXCLUDED from baseline T07, never PASS
by simulation. It is an additional production-hardening or enterprise security
profile: a deployment selecting that profile needs independent human custody
evidence before claiming it. Final evidence must distinguish these two scopes.
A green Actions job is not T07 closure. Per-phase checkpoints are
written only after assertions pass. Missing checkpoints are NOT RUN; preserve
failures, original run/attempt and exact SHA across retries.

The strict reload recovery probe still fails if automatic completion does not
occur. A separate `recovery-boundary.json` can classify that exact command as
`EXPECTED-UNKNOWN` under the stable exclusion only after read-only evidence
checks: unique matching journal, no authenticated terminal receipt/root result,
unchanged reload count, query-only recovery frames, fresh owner fence and no
subsequent conflicting writes. It never changes the failed strict result,
replays the mutation, edits durable state or upgrades missing evidence to PASS.

Retain the artifact and its GitHub digest before expiry, with product digests,
manifest, environment inventory, timestamps, exit status and API/DB/journal/root
receipt evidence. Logs are private until redacted. Never upload the workspace
secret directory, cookies, raw database or password file. Cleanup removes only
task containers, network namespace, builder and private directory; the native
installation lives only on the disposable hosted runner. Do not generalize this
cleanup to a shared host.

To close T07, cover the prepublication production installation paths with the
reviewed signed candidate, separately authenticated principals and provisioned
external services; obtain real positive and critical rejection evidence for
every promised workflow. Publication-dependent immutable download paths belong
to T09/formal release. Positive configuration apply requires native acceptance
of the [reviewed complete contract](complete-config-contract.md), not just
contract approval or unit tests; missing acceptance still blocks readiness. Preserve the
finite compatibility exclusions and matched-release recovery boundary in
[stable contracts](../reference/stable-contracts.md). Do not import earlier T03,
T04, T05 or T06 results as runtime acceptance of the new candidate.

Freeze the final candidate SHA/tree only after product, deployment, contract
and probe edits stop. Keep later evidence outside the repository. Every final
result must name that SHA, never splice artifacts from earlier commits or
attempts. Changes to Compose, the verification-key contract and native units
require a fresh complete `release-upgrade.yml` run from transitional v1.0.0 with
`version=1.0.1`, `candidate_sha` equal to the frozen SHA and
`session_compatibility=false`, `session_only=false`, `business_only=false`.
All four native Agent/Controller architecture jobs and the aggregate must pass
in the same run/attempt. Business success does not replace this gate.
Pre-1.0 installations must redeploy into `1.x`; their historical diagnostic
results are not upgrade support or acceptance of this production baseline.

Existing [single-Relay](single-relay-validation.md) and
[cross-VM enrollment](real-e2e.md) profiles retain their original scope.
