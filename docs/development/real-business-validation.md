# Business Smoke and Integration

The release Business Smoke job exercises one
signed amd64 production path: Controller and native systemd Agent/privd/ocserv
installation, Local requester/approver login, approved ConfigPlan apply, real
OpenConnect VPN traffic, automatic ConfigPlan rollback and a fresh VPN check.
No production binary, service, Relay or VPN is mocked. This is neither Package & Upgrade
upgrade acceptance nor G6 production readiness.

Integration is a change-selected specialized check. It retains the original OIDC, PKI/P12/revoke, real-browser,
single-Relay fault/recovery and cross-source evidence checks until equivalent
owners are established. See the [coverage inventory](release-business-coverage.md).
The [Release Check](release-checks.md) determines the required scope from the
complete published-release-to-candidate diff; unselected Integration is not evaluated. Do not describe
smoke as a 5-15 minute job until its measured timings justify that target.

With explicit authorization to use disposable GitHub-hosted runners, dispatch
`release-upgrade.yml` on the exact candidate branch with `version`,
`baseline_release=v1.0.0` and `purpose=smoke` or `purpose=integration`.
The candidate SHA comes from the actual dispatch context. The baseline
input is unused in this mode; the ordinary native upgrade remains the default.
This mode does not publish packages/images, create a tag, or modify Secrets.
Local preparation checks must run in an isolated checkout on `BuildServer`.
Never run the business driver or its host-installing steps on BuildServer.

## Evidence and boundaries

The release jobs consume the exact producer artifacts after explicit digest
checks. Standalone diagnostics can build their own matched candidate.
The driver then uses `controller.sh install --release-file`.
The same independently held task signing key verifies the native package before
`dpkg`; the embedded payload verifier remains enabled. Managed-node preparation
and the final `SERVICES_ACTIVE` convergence use the shipped installer.
After the approved positive ConfigPlan apply, each profile establishes
an OpenConnect tunnel and passes real ICMP traffic. It then applies a reviewed
revision whose reload is deliberately rejected, verifies automatic exact-byte
rollback, and establishes a new OpenConnect tunnel with ICMP traffic. This is
an **automatic rollback of a failed apply**, not an operator-initiated rollback
of the successful revision. `timings.json` records per-stage wall-clock seconds;
the job summary records artifact-upload seconds, URL and SHA-256 digest. No runtime
target is currently a release contract.

`result.json` records scope, candidate identity, outcome, timings and passed
checkpoints once. GitHub job status is the authoritative workflow result;
no second evidence manifest or cross-candidate inheritance is used.

The following limitations are deliberately retained, not scored as PASS:

- An unpublished candidate cannot exercise the fixed GitHub Release download
  and versioned Controller bootstrap. This publication-dependent evidence is
  deferred to Publish, not a perpetual prepublication Business Smoke blocker.
  Signed candidate lifecycle and managed-node preparation/convergence remain
  Business Smoke requirements. No fabricated tag, download stub, unsigned substitution or
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
- In the extended profile, the dedicated HTTPS OIDC fixture exercises discovery, PKCE, callback and
  issuer/signature/nonce/code/state rejection in mixed mode, plus OIDC-only
  login, workspace authorization and restart/logout behavior. The signer uses
  actual OpenSSL CSR verification, signing, public-key sealing and revocation
  acknowledgement over authenticated TLS. These are task-controlled external
  service fixtures, not production IdP/CA/HSM certification or independent
  operator custody. The Controller container trust store is provisioned with
  the task CA; TLS verification stays enabled.
- Extended certificate/P12 checks include node restarts and one-use download.
  The real browser uses the signed gateway and Controller API without route
  mocks, simulator or TLS exceptions. Missing phase checkpoints remain NOT RUN;
  source implementation is not proof of runtime success. The reviewed complete
  node-local TLS profile is exercised through browser plan/approval/apply and
  native exact-byte rollback/restart checks; only their checkpoints prove that
  a particular candidate passed.
- One host, one architecture and one Relay provide no Relay redundancy or Resilience
  fault-domain/SLO proof. The client namespace protects host routes; it is not
  another host. The probe checks test VPN traffic, never real user traffic.

`result.json` reports `scope=business-smoke` or `scope=integration`.
It never reports full release acceptance. Both record
`operator_mode=simulated_two_principals` and human custody `NOT_VERIFIED`, even
when the probe passes. Baseline 1.0 and Business Smoke require independently controlled
requester and approver principals, not an organizational two-person rule.
Automated acceptance may authenticate and exercise both principals separately;
it must preserve the RBAC, binding, self-approval and replay checks above.
Actual two-person credential custody is EXCLUDED from baseline Business Smoke, never PASS
by simulation. It is an additional production-hardening or enterprise security
profile: a deployment selecting that profile needs independent human custody
evidence before claiming it. Final evidence must distinguish these two scopes.
A green smoke job proves only the smoke scope, not the extended or other release
gates. Per-phase checkpoints are
written only after assertions pass. Missing checkpoints are NOT RUN; preserve
failures, original run/attempt and exact SHA across retries.

The extended profile's strict reload recovery probe still fails if automatic completion does not
occur. A separate `recovery-boundary.json` can classify that exact command as
`EXPECTED-UNKNOWN` under the stable exclusion only after read-only evidence
checks: unique matching journal, no authenticated terminal receipt/root result,
unchanged reload count, query-only recovery frames, fresh owner fence and no
subsequent conflicting writes. It never changes the failed strict result,
replays the mutation, edits durable state or upgrades missing evidence to PASS.
It remains in extended acceptance pending an explicit equivalence mapping to G6; the presence
of G6 fault-domain recovery alone is not sufficient to delete this distinct
single-Relay assertion.

Retain the artifact and its GitHub digest before expiry, with product digests,
manifest, environment inventory, timestamps, exit status and API/DB/journal/root
receipt evidence. Logs are private until redacted. Never upload the workspace
secret directory, cookies, raw database or password file. Cleanup removes only
task containers, network namespace, builder and private directory; the native
installation lives only on the disposable hosted runner. Do not generalize this
cleanup to a shared host.

To close the smoke scope, cover the prepublication production installation path
with the reviewed signed candidate, separately authenticated principals,
positive ConfigPlan apply, automatic native rollback and VPN traffic before and
after rollback. Keep the extended production-path checks until their coverage
is formally transferred. Publication-dependent immutable download paths belong
to Publish. Positive configuration apply requires native acceptance
of the [reviewed complete contract](complete-config-contract.md), not just
contract approval or unit tests; missing acceptance still blocks readiness. Preserve the
finite compatibility exclusions and matched-release recovery boundary in
[stable contracts](../reference/stable-contracts.md). Do not import earlier T03,
T04, T05 or Package & Upgrade results as runtime acceptance of the new candidate.

Freeze the candidate before the release workflow. A new candidate cannot borrow
old product/test results. Independent architecture failures may be rerun with
producer-bound inputs; shared fault timelines must be rerun together.
The supported v1.0.0 native upgrade and mixed-version application cells remain
fixed release requirements. Business success does not replace them.
Pre-1.0 installations must redeploy into `1.x`; their historical diagnostic
results are not upgrade support or acceptance of this production baseline.

Existing [single-Relay](single-relay-validation.md) and
[cross-VM enrollment](real-e2e.md) profiles retain their original scope.
