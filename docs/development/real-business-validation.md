# Business Smoke and Integration

The release Business Smoke job exercises one
signed amd64 production path: Controller and native systemd Agent/privd/ocserv
installation, Local requester/approver login, approved ConfigPlan apply, real
OpenConnect VPN traffic, automatic ConfigPlan rollback and a fresh VPN check.
No production binary, service, Relay or VPN is mocked. This is neither native
package acceptance nor a Controller/database HA guarantee.

Integration is a change-selected specialized check. It retains the original OIDC, PKI/P12/revoke, real-browser,
single-Relay recovery, Agent/privd restart and cross-source evidence checks. See the [coverage inventory](release-business-coverage.md).
The [Release Check](release-checks.md) determines the required scope from the
complete published-release-to-candidate diff; unselected Integration is not evaluated. Do not describe
smoke as a 5-15 minute job until its measured timings justify that target.

With explicit authorization to use disposable GitHub-hosted runners, dispatch
`release-upgrade.yml` on the exact candidate branch with `version`,
and `purpose=smoke` or `purpose=integration`. Add `run-resilience=true` for
Controller, Agent/privd/transport, database API outage and sole-Relay recovery.
The extended profile reuses its Agent/privd and Relay scenarios once.
The candidate SHA comes from the actual dispatch context. There is no baseline
input or historical upgrade mode.
This mode does not publish packages/images, create a tag, or modify Secrets.
Local preparation checks run in an isolated environment with existing tools.
Native host-installing steps run only on a disposable runner.

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
- Selected single-instance recovery provides no HA, Relay redundancy or
  production RTO/RPO guarantee. The client namespace protects host routes; it is not
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

The offline Relay queue scenario proves the command was never sent, then
requires the original operation to succeed with exactly one additional native
reload after restoring the same Relay. No EXPECTED-UNKNOWN exception can turn
its failure into acceptance. Other uncertain outcomes retain read-only
reconciliation and the existing explicit safe retry after proven effect absence.

`result.json` records `resilience_requested`, `resilience_result` and actual
scenario checkpoints. A selected recovery run requires exactly one PASS for
Controller, Agent/privd, transport, reference database API outage, Relay and
completion. Missing fields/checkpoints, cancellation, failure and unexpected
skip block acceptance. Unselected recovery is explicitly SKIPPED. Install-only
architecture diagnostics are SKIPPED and do not supply recovery acceptance.

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
matched-release recovery boundary in
[stable contracts](../reference/stable-contracts.md). Do not import earlier T03,
T04, T05 or Package & Upgrade results as runtime acceptance of the new candidate.

Freeze the candidate before the release workflow. A new candidate cannot borrow
old product/test results. Independent architecture failures may be rerun with
producer-bound inputs; shared fault timelines must be rerun together.
Historical native upgrade and mixed-version application cells are retired.
Business success does not replace retained current-product, security or
resilience gates. Historical results are not acceptance of this candidate or a
cross-version safety guarantee.

Existing [single-Relay](single-relay-validation.md) and
[cross-VM enrollment](real-e2e.md) profiles retain their original scope.
