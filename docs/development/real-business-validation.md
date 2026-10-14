# Business Smoke and Integration

Business Smoke runs the real Controller, native systemd Agent/privd/ocserv
services, Local requester and approver browser sign-in, Workspace selection,
VPN user creation, approval and reload, approved ConfigPlan apply, and an
OpenConnect VPN session on a single disposable amd64 runner with one
Controller and one dedicated Relay. Every production binary, service, Relay,
and VPN path runs without mocks. Setting `production_signer=true` (the default
in Release Check and `release-upgrade.yml`) enables production Signer sealing
and internal TLS, whereas `false` switches to the standalone topology without
them. Smoke also provisions a separate API-created VPN user and, when
`production_signer=true`, verifies that the production Signer seals that user's
password.

The manual integration profile expands on Smoke by exercising OIDC, PKI/P12
issuance and revocation (including browser flows), rollback, and deeper fault
recovery. Before running the VPN and recovery stages, the harness applies the
node configuration once through the browser and checks the resulting native
file hash against the operation receipts. Both Smoke and Integration require
`real_browser_subset`, and any missing browser checkpoint fails the run. While
driving the UI, the browser runner captures Content Security Policy violations
directly from the page (the gateway enforces CSP without a report endpoint) and
writes query-stripped entries to `browser-csp.json`. Acceptance fails if any
page violation or console CSP error appears outside the final positive control,
if the collector misses that control, or if the control is only reported
without being enforced.

Dispatch commands, topology selection, coverage ownership, and release
publication gates are documented in [Release policy](release-checks.md). On its
own, passing Business validation does not certify native package distribution
or high availability.

## Evidence and boundaries

Instead of pulling from external release endpoints, the Business driver builds
native packages and container images from the candidate tree, pushes images to
a loopback test registry, and runs `controller.sh install --release-file` with
standard JSON configuration. In the extended profile with the production
Signer, Integrated configuration upgrades and rollbacks test both digest and
version-tag references against those locally built images, without testing
compatibility across historical binary releases. Native package scriptlets
validate the embedded archive checksum inside root-owned staging, and
managed-node preparation through final `SERVICES_ACTIVE` convergence runs
through the shipped installer.

Once the approved positive ConfigPlan apply finishes, both profiles bring up an
OpenConnect tunnel and verify end-to-end ICMP traffic. Manual integration then
submits a reviewed configuration revision engineered to fail during ocserv
reload, confirms that the node automatically restores the previous files byte
for byte, and establishes a fresh OpenConnect tunnel with working ICMP traffic.
Note that this stage tests automatic rollback when an apply fails on the node,
as opposed to an operator-initiated rollback of an already active revision.

In its final stage, manual integration builds the next patch version of the
Agent package, drops it into the node's root-owned upgrade spool, and recreates
the Controller container with a diagnostics-only Compose override that mounts a
release catalog (a mount absent from the shipped launcher). Through the
browser, the harness initiates and approves a one-node canary rollout to that
patch version. For the stage to pass, the rollout must complete, consume the
approval, leave both the installed binaries and the live Agent reporting the
target version, and record zero unexpected CSP violations. Stage wall-clock
durations are written to `timings.json`, and sanitized diagnostic bundles preserve
the execution environment and outcome, though no runtime duration target acts as
a release gate.

Workflow pass/fail authority rests with the GitHub Actions job status, while
`result.json` records the selected scope, diagnostic schema version, overall
outcome, stage timings, and completed checkpoints.

Keep the following scope boundaries in mind when interpreting results; the
harness records them as explicit limitations and never scores them as PASS:

- Building and installing from local artifacts leaves the public GitHub Release
  download endpoint untested; Stage-0 download/failure fixtures and Release
  build-only smoke tests cover that path separately. Business exercises the
  native package lifecycle and managed-node convergence without fabricating a
  published release version.
- The harness provisions the private Relay CA through the documented protected
  public CA files, and the shipped Compose overlay and native launcher enable
  the existing strict-TLS binary flag without patching descriptors or launcher
  scripts. The managed-node installer drives enrollment and writes the final
  node configuration while keeping preparation, PENDING_APPROVAL, and
  SERVICES_ACTIVE as distinct checkpoints. Smoke registers through the standard
  path: a `POST /api/v1/node-bootstrap-tokens` token without an EndpointID,
  handed to the first installer run as a protected `BOOTSTRAP_TOKEN_SOURCE`,
  reaches `ENROLLED_LOCAL` in that run. The harness then checks that the
  Controller stored only the token digest, bound it to the Agent's EndpointID
  on consumption, and kept the node pending, and that installer reruns while
  pending and after activation keep the same node ID and identity files.
  Extended keeps the legacy endpoint-bound two-phase registration
  (`ENROLLMENT_READY`, then `POST /api/v1/enrollment-tokens`).
- For Local authentication, the requester and approver log in as separate
  principals with distinct credentials and isolated browser sessions, without
  relying on devAuth or sessions injected directly into the database (direct
  database writes are limited to initial workspace provisioning before Local
  bootstrap). The suite checks RBAC, workspace authorization, 403 responses on
  self-approval, approval content binding, and replay rejection across those
  two principals, though automated execution by one runner cannot prove
  independent physical custody by two human operators.
- In the extended profile, a dedicated HTTPS OIDC fixture tests discovery,
  PKCE, callback handling, and rejection of invalid issuers, signatures,
  nonces, authorization codes, and state parameters in mixed mode, followed by
  OIDC-only sign-in, workspace authorization, and restart/logout behavior. The
  Signer exercises real OpenSSL CSR verification, certificate signing,
  public-key sealing, and revocation acknowledgements over authenticated TLS,
  with the Controller container trusting the task CA while keeping TLS
  verification enabled. Because the OIDC provider and Signer CA run as
  task-controlled fixtures, they do not certify third-party production
  IdP/CA/HSM integrations or multi-operator key custody.
- Extended certificate and P12 checks cover node restarts and single-use bundle
  downloads. Across both profiles, a real browser drives the built gateway and
  Controller API without route mocks, transport simulators, or TLS verification
  bypasses. Any unexecuted phase remains NOT RUN, and only recorded runtime
  checkpoints (including browser plan, approval, and apply plus native
  exact-byte rollback and restart for the node-local TLS profile) count as
  evidence for a candidate commit.
- Smoke recovery verifies service readiness, fresh Controller and Agent
  sessions, preserved node identity, and working business operations after
  reconnection, while manual integration adds outage queue semantics and
  durable receipt checks. These single-instance recovery checks do not provide
  high availability, Relay redundancy, or production RTO/RPO guarantees. The
  OpenConnect client runs inside an isolated network namespace on the same
  runner to protect host routing tables, exercising synthetic test traffic on
  one machine without a separate client host or live user traffic.
- Both profiles end with a terminal revocation of the real node (in Extended,
  after the rollout). Anonymous, foreign-Origin, unapproved,
  stale-approval, and pending-approval requests, and the approver spending the
  requester's approval, are all rejected while the node stays active. The
  approved revocation must return 200, meaning the request itself applied the
  revoked trust and the connection close. It passes only after the approval is
  consumed, the node and its endpoint key read revoked, the revoked trust
  convergence intent is queued at that revision, no owner lease remains, and
  the node reads offline while the same Agent process stays up and logs the
  ended Controller connection. The old reload approval then creates no
  operation and leaves privd effects, the Agent journal, ocserv reloads, and
  node files unchanged. A 503 (synchronization left to the background worker)
  or a timeout fails the run.

In `result.json`, the harness writes `scope=business-smoke` or
`scope=integration` (never full release acceptance), along with
`operator_mode=simulated_two_principals` and human custody `NOT_VERIFIED`.
Both principals must still pass all RBAC, content-binding, self-approval, and
replay assertions; two-person human custody requires separate evidence under
the [approval boundary](../reference/stable-contracts.md#approval-principal-boundary).
Checkpoints are written only after their assertions succeed, leaving unreached
phases as NOT RUN. Across retries, retain any failed runs, the original run
and attempt numbers, and the exact candidate SHA.

In the manual integration offline-Relay scenario, the harness first proves that
the queued command was never transmitted while the Relay was down, and then
requires the original operation to finish with exactly one additional native
ocserv reload once the same Relay comes back online. An EXPECTED-UNKNOWN
classification cannot convert a failure in this scenario into a pass. For other
interrupted operations whose side effects are genuinely uncertain, the system
relies on read-only reconciliation followed by an explicit safe retry once
absence of effect is confirmed.

Recovery reporting in `result.json` includes `resilience_requested`,
`resilience_result`, and individual scenario checkpoints. When recovery is
enabled, acceptance requires a single PASS for each of the four scenarios: the
Controller stack (which in Smoke includes transport), the Agent/privd stack,
bundled PostgreSQL, and the sole Relay; there is no Business-level MySQL
recovery scenario. Missing fields or checkpoints, job cancellation, test
failures, or unexpected skips block acceptance, while runs invoked without
recovery record SKIPPED.

Sanitized diagnostic artifacts keep the environment inventory, stage timings,
and exit status for seven days on both passing and failing runs, and manual
integration adds observations from the API, database, Agent journal, and root
receipts. Keep logs private until redaction completes, and never upload the
workspace secret directory, session cookies, raw database dumps, or password
files. At the end of a run, cleanup removes only task-created containers, the
client network namespace, builder state, and the private working directory;
because the native package installation mutates the disposable hosted runner
itself, never apply this cleanup procedure to a shared machine.

Finally, passing positive ConfigPlan apply requires native execution of the
[complete contract](configuration.md#complete-profile-contract) beyond schema review or
unit tests, and recovery validation must respect the
[matched-release recovery boundary](../reference/stable-contracts.md#matched-release-recovery-boundary).
When a check fails, rerun the owning suite with its complete check set and
shared fault timeline on the current candidate commit, as results from earlier
candidates cannot fill in missing acceptance or establish cross-version safety.
The standalone [single-Relay](single-relay-validation.md) and
[cross-VM enrollment](cross-vm-enrollment-validation.md) procedures retain their
existing scope.
