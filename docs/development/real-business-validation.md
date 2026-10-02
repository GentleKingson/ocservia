# Business Smoke and Integration

Business Smoke exercises the real Controller, native systemd Agent/privd/ocserv,
Local requester/approver login, approved ConfigPlan apply, OpenConnect VPN,
production Signer sealing and internal TLS on disposable amd64 runners.
Manual integration adds OIDC, PKI/P12/revoke, browser, rollback and deep recovery.
No production binary, service, Relay or VPN is mocked. Use
[Release policy](release-checks.md) for dispatch commands, topology selection,
coverage ownership and publication gates; Business alone establishes neither
native package acceptance nor HA.

## Evidence and boundaries

The Business driver builds its own native packages and images, uses a loopback
test registry, and runs `controller.sh install --release-file` with ordinary
JSON configuration. Integrated configuration upgrade/rollback exercises digest
and version-tag references to the same locally built images; it does not test
historical binary compatibility. Native scriptlets verify the embedded archive
checksum in root-owned staging. Managed-node preparation and final
`SERVICES_ACTIVE` convergence use the shipped installer.
After the approved positive ConfigPlan apply, each profile establishes
an OpenConnect tunnel and passes real ICMP traffic. Manual integration then applies a reviewed
revision whose reload is deliberately rejected, verifies automatic exact-byte
rollback, and establishes a new OpenConnect tunnel with ICMP traffic. This is
an **automatic rollback of a failed apply**, not an operator-initiated rollback
of the successful revision. `timings.json` records per-stage wall-clock seconds;
sanitized diagnostics retain the actual environment and execution outcome. No
runtime target is currently a release contract.

`result.json` records scope, diagnostic version, outcome, timings and passed
checkpoints. GitHub job status is the authoritative workflow result.

The following limitations are deliberately retained, not scored as PASS:

- Local builds do not test the public GitHub Release download endpoint. Stage-0
  download/failure fixtures and Release build-only smoke have separate owners.
  Business runs the actual native lifecycle and managed-node convergence;
  it does not create a fabricated published version.
- The private Relay CA is provisioned through the documented protected public
  CA files. The shipped Compose overlay and native launcher use the existing
  strict-TLS binary option without descriptor/launcher edits. Enrollment and
  its final node configuration are produced by the managed-node installer;
  preparation, PENDING_APPROVAL and SERVICES_ACTIVE checkpoints remain separate.
  This does not turn the locally installed package into a published Release
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
  The real browser uses the built gateway and Controller API without route
  mocks, simulator or TLS exceptions. Missing phase checkpoints remain NOT RUN;
  source implementation is not proof of runtime success. The reviewed complete
  node-local TLS profile is exercised through browser plan/approval/apply and
  native exact-byte rollback/restart checks; only their checkpoints prove that
  a particular candidate passed.
- Smoke recovery checks readiness, fresh Controller/Agent sessions, unchanged node
  identity and successful business after reconnect. Manual integration retains
  outage queue semantics and durable receipt checks. Selected single-instance
  recovery provides no HA, Relay redundancy or
  production RTO/RPO guarantee. The client namespace protects host routes; it is not
  another host. The probe checks test VPN traffic, never real user traffic.

`result.json` reports `scope=business-smoke` or `scope=integration`, never full
release acceptance. Both record `operator_mode=simulated_two_principals` and
human custody `NOT_VERIFIED`. Independently authenticated principals must pass
RBAC, binding, self-approval and replay checks. Two-person custody is excluded
and needs separate evidence under the
[approval boundary](../reference/stable-contracts.md#approval-principal-boundary).
Write checkpoints only after assertions pass. Missing checkpoints are NOT RUN;
retain failures, original run/attempt and exact SHA across retries.

The manual integration offline Relay queue scenario proves the command was never sent, then
requires the original operation to succeed with exactly one additional native
reload after restoring the same Relay. No EXPECTED-UNKNOWN exception can turn
its failure into acceptance. Other uncertain outcomes retain read-only
reconciliation and the existing explicit safe retry after proven effect absence.

`result.json` records `resilience_requested`, `resilience_result` and actual
scenario checkpoints. A selected recovery run requires exactly one PASS for
Controller stack (including transport), Agent/privd stack, PostgreSQL and Relay. Missing fields/checkpoints, cancellation, failure and unexpected
skip block acceptance. Unselected recovery is explicitly SKIPPED.

Sanitized diagnostics retain environment inventory, timings and exit status
for seven days on success and failure. Manual integration also retains
API/DB/journal/root receipt observations. Logs are private until redacted. Never upload the workspace
secret directory, cookies, raw database or password file. Cleanup removes only
task containers, network namespace, builder and private directory; the native
installation lives only on the disposable hosted runner. Do not generalize this
cleanup to a shared host.

Positive ConfigPlan apply requires native acceptance of the
[complete contract](configuration.md#complete-profile-contract), not just contract review or
unit tests. Preserve the [matched-release recovery boundary](../reference/stable-contracts.md#matched-release-recovery-boundary).
Rerun failed owners with complete checks and shared fault timelines together;
earlier candidates cannot supply missing acceptance or cross-version safety.

Existing [single-Relay](single-relay-validation.md) and
[cross-VM enrollment](real-e2e.md) profiles retain their original scope.
