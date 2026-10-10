# Architecture

ocservia provides a control plane for OpenConnect (ocserv) VPN gateways. The gateways still terminate client tunnels and forward payload traffic independently. ocservia shifts node management to a central Controller that handles cluster visibility, user and group provisioning, configuration distribution, node enrollment, software updates, audit logging, and state recovery.

## Purpose and constraints

Operators need to verify node trust and reachability, push configurations without interactive host access, and trace commands when their outcomes are ambiguous. High-risk changes require independent approval by a second authenticated principal. During disaster recovery, node identities and the logs of state transitions must remain intact.

The supported production topology uses one Controller instance, a relational database, and one dedicated Relay that serves multiple Agents (a non-empty second Relay URL is rejected). Certificate issuance uses a separate Signer service. These components can run on distinct hosts; Integrated mode puts the Controller, Relay and Signer on one host. High-availability setups like multi-master Controller clusters, redundant Relays, or automated database failovers are outside the [supported scope](reference/support-policy.md#deployment-capability-distinctions). Internal lease coordination and service modularity do not change this boundary.

The system separates the data plane from the control plane: client VPN sessions terminate at the ocserv instances and payload never transits the Controller, transportd or Relay. Loss of the control plane stops administration, validation and new commands; it is not in the tunnel payload path, but local node crashes or flawed ocserv reloads still disrupt traffic. A deployment requires pre-provisioned cryptographic credentials, functional DNS and TLS, and a certified database instance. Certificate management requires a Signer service, and single sign-on workflows need an OpenID Connect (OIDC) identity provider.

## High-level view

```mermaid
flowchart LR
    Operator["Operator browser"] -->|HTTPS| Gateway["Gateway: Web bundle and /api proxy"]
    Gateway --> Controller["Go Controller"]
    Controller --> Database[("Supported database and backups")]
    Controller -->|HTTPS| Signer["Signer"]
    Controller <-->|gRPC over UDS| Transportd["transportd"]
    Transportd <-->|"Iroh, direct or via the one Relay"| Agent["Agent"]
    Agent -->|UDS| Privd["privd"]
    Privd --> Ocserv["Local ocserv server"]
```

## Main pieces

| Piece | Runs on | What it does |
| --- | --- | --- |
| Gateway | Controller server | Terminates Controller TLS, serves the static Web bundle and proxies `/api` to the Controller. Integrated mode adds an SNI-routing Edge in front of it. |
| Go Controller | Controller server | Serves the HTTP API and runs the outbox worker, scheduler, audit subsystem, node directory and command dispatch pipeline, selected by `--role`. |
| transportd | Controller side | Terminates Iroh overlay connections and exposes an IPC interface over a local gRPC Unix domain socket to the Go Controller. It operates without database credentials, relying on a trust socket served by the Controller for session authorization. |
| Supported database | Controller side | Persists control-plane state, task queues, and recovery journals. PostgreSQL is the reference backend. |
| Dedicated Relay | Operator-managed relay host (one) | Forwards encrypted control-plane traffic between Controller and Agents across network boundaries. Production supports one Relay, not a redundant set. |
| Signer | Separate service (bundled in Integrated mode) | Issues and revokes node client certificates and seals secrets to node public keys. See [Production Signer](development/certificates-and-signer.md#production-signer). |
| Managed node service | Each ocserv server | Connects to the Controller, streams telemetry and heartbeats, accepts validated commands, and logs execution status. |
| Local privileged helper | Each ocserv server | Runs a fixed set of ocserv lifecycle routines that require root privileges. |
| Upgrader | Each managed node | Manages package installation and binary updates independently of the Agent execution loop. |
| ocserv | Each VPN server | The underlying OpenConnect VPN daemon handling client encapsulation and cryptographic tunneling. |
| External services | Operator environment | Provide external infrastructure for federated authentication, telemetry collection, secret storage, and backups. |

Ownership, secrets, interfaces and failure impact by process:

| Process | Storage and secrets | Interfaces | If it stops |
| --- | --- | --- | --- |
| Gateway | Controller TLS key; no business state | TCP 443 to browsers; HTTP to the Controller | Web and API unreachable; Controller state and nodes unaffected. |
| Go Controller (`--role=all`, single instance in production) | Authority for trust, operations, outbox and audit in the database; holds the command-signing key, session and audit keys, database application credential and Signer token | HTTP behind Gateway; client of the transportd socket; serves the trust socket; HTTPS to Signer | No validation, durable writes or dispatch; queued operations wait in the outbox. |
| Database | The only durable Controller state | Controller only | Same as Controller; restore per [database recovery](operations/incident-recovery.md#database-recovery). |
| transportd | Iroh endpoint key, Relay token and Controller command verification key; no database credentials | gRPC `TransportService` on a Unix socket; calls the trust socket; Iroh to Agents | Dispatch and result ingestion stop; outcomes may become `unknown` and are reconciled. |
| Signer | Issuing intermediate key, HTTPS key and the durable certificate ledger | HTTPS `:9443`, on an internal network reachable by the Controller in Integrated mode | Certificate issuance, revocation and secret sealing fail; other operations continue. |
| Relay | Relay TLS key and the shared token; no control-plane authority | HTTPS and UDP 7842 | Relay-dependent paths are interrupted; existing direct paths may continue. VPN payload is not carried by it. |
| Agent | Local SQLite journal; Controller verification key | Outbound Iroh; Unix socket to privd | No commands or telemetry for that node; the journal must survive restarts. |
| privd | Root effect store, attestation key, Controller keyring and the two sealing private keys | `/run/ocserv-platform/privd.sock`, no TCP listener | Privileged commands fail or stay indeterminate. |
| upgrader | Runs as the one-shot `ocservia-upgrader@<operation>.service` started by privd | systemd | Upgrade does not progress; see [Agent lifecycle](operations/agent-lifecycle.md). |

At runtime, the Go control plane communicates with `transportd` through a local Unix domain socket. `transportd` and remote `Agent` daemons exchange versioned Protocol Buffer payloads over an Iroh overlay, directly or through the Relay. On the nodes, the `Agent` delegates privileged actions to `privd` via a dedicated Unix domain socket. The transportd control and trust sockets and the privd socket each check the peer UID and socket ownership. Intermediate Relay nodes forward encrypted packets without acquiring control-plane authority. In development, `transportd-stub` and `OCSERV_LOCAL_SIMULATOR` (forbidden in production) replace the Iroh path; production uses Iroh. For wire protocols and process isolation semantics, see [transport](development/transportd.md) and [Agent/privd](development/agent-privd.md).

## How deployment fits together

Target environments must fall within the [production support matrix](operations/production-deployment.md#database-support). Test harness configurations do not carry production guarantees. Backup semantics and disaster recovery procedures vary by database engine, as explained in [database recovery](operations/incident-recovery.md#database-recovery).

1. Provision the Controller on a verified Linux distribution.
2. Configure environment parameters in `install.env` and inject cryptographic secrets out-of-band to keep them out of the repository checkout.
3. Set up a dedicated Relay instance to route control-plane network traffic.
4. Distribute and install the node agent package across target ocserv hosts.
5. Run the cryptographic enrollment workflow for each host, verify the pending node in the Controller, and start the node daemons.
6. Administer node fleets, monitor telemetry, enforce access policies, and schedule configuration changes via the HTTP API or web console.

## How a node change is applied

```text
Operator request
  -> Controller validates and records the request
  -> high-risk changes wait for independent approval
  -> Controller sends a signed, fixed operation to the node
  -> node runs the matching local ocserv action
  -> result and audit record return to the Controller
```

Command processing decouples request ingestion from remote execution:

1. Ingestion and transactional staging: The API handler maps the request to the domain service. Inside an isolated database transaction, the service verifies node state and operator permissions, then commits the operation intent, payload, transactional outbox record, and audit entry. This transactional acceptance logs the intent; it does not mean the node has executed it.
2. Outbox dispatch: An asynchronous worker locks and claims pending outbox tasks in batches, then invokes `transportd`. Dispatch occurs across an active session under the transport session identity. Network transmission updates the dispatch tracking but does not confirm the operation succeeded.
3. Node admission and idempotency: The `Agent` validates the payload and writes the command identifier to an embedded SQLite journal before running it. If a command with the same identifier and semantic hash arrives again, `Agent` skips execution and replays the cached result. Any payload mismatch under an existing identifier results in immediate rejection.
4. Privileged execution and settlement: For mutating operations, `Agent` transitions the task state to `running` before invoking `privd` over local IPC. `privd` validates the parameters against fixed authorization rules and issues signed execution receipts upon completion. `Agent` writes the terminal status to disk before returning an acknowledgment. The Controller worker receives the result through the transportd event stream (`localslice.Service.Ingest`, which also ingests telemetry) and commits it, making the outcome available to API queries and Server-Sent Event (SSE) streams.

This lifecycle runs across the [operation service](../control-plane/internal/operations/service.go), the [outbox worker](../control-plane/internal/operations/worker.go), and the [Agent execution path](../rust/crates/agent/src/main.rs). The [delivery and recovery contract](development/command-reliability.md) governs idempotency, result verification, and conflict reconciliation. Because local database commits, network transit, and daemon side-effects lack a unified distributed transaction coordinator, communication failures or crashes may leave operations in an `unknown` state. Automated recovery procedures must reconcile ambiguous executions against node receipts before retrying; daemon status queries alone cannot confirm whether a transient reload completed.

### Code path of a controlled node operation

The service reload action shows the source owner at each hop. Other controlled
node operations share the Controller half; their node-side handlers differ.

| Hop | Entry point | Owner and boundary |
| --- | --- | --- |
| Browser | [`NodeDetailView.vue`](../web/src/views/NodeDetailView.vue) → [`shared/fleet.ts`](../web/src/shared/fleet.ts) `reloadService` → [`api/operations.ts`](../web/src/api/operations.ts) `reloadService` → generated client → [`api/transport.ts`](../web/src/api/transport.ts) | Fleet store owns tracking; the adapter supplies `Idempotency-Key`, `If-Match` and approval headers. Hidden buttons are not authorization. |
| HTTP | [`api/routes.go`](../control-plane/internal/api/routes.go) `POST /api/v1/nodes/{node_id}/service:reload` → [`requireOperationAuth`](../control-plane/internal/api/authorization.go) → [`createControlledCommand`](../control-plane/internal/api/operations.go) | Authentication, browser-origin check and route RBAC, then request validation; returns `202` with the operation, not an execution result. |
| Acceptance | [`operations.Service.CreateSynthetic`](../control-plane/internal/operations/service.go) | One database transaction checks the expected node version, consumes the bound approval and stages operation, outbox and audit. |
| Dispatch | [`operations.Worker.dispatch`](../control-plane/internal/operations/worker.go) → [`Service.Claim`](../control-plane/internal/operations/dispatch.go) → `dispatchFenced` → [`transportclient`](../control-plane/internal/transportclient/) | Claims up to 16 jobs and sends them serially under the owner fence; `MarkSentWithEnvelope` records transmission only. |
| Transport | [`transportd`](../rust/crates/transportd/src/lib.rs) `send_command` | Unprivileged; forwards over the authorized Iroh session. |
| Node | [`agent`](../rust/crates/agent/src/lib.rs) `CommandExecutor` → [`command-journal`](../rust/crates/command-journal/src/lib.rs) `accept_command` → `PrivdClient` → [`privd`](../rust/crates/privd/src/lib.rs) `dispatch_attested` | The journal records acceptance before execution; privd runs the fixed action and signs the receipt. |
| Result | [`localslice.Worker`](../control-plane/internal/localslice/) → [`Service.Ingest`](../control-plane/internal/localslice/ingress.go) | Validates each transport event and commits its result or telemetry in one transaction; ambiguous outcomes are reconciled per [command reliability](development/command-reliability.md). |

Privileged execution on managed nodes is restricted to a set of pre-compiled subroutines. The system disallows arbitrary binaries, parameterized shell strings, dynamic service identifiers, or unconstrained file paths.

Node enrollment operates via an isolated protocol. An ephemeral pre-shared token links a node's persistent `EndpointID` with a pending inventory entry. An administrator approves the registration, which lets the local `Agent` negotiate an authenticated transport session. Package deployment and service initiation do not grant connectivity before this handshake. For specifications, see [enrollment](how-to/enroll-node.md) and the [Controller service](../control-plane/internal/enrollment/service.go).

## State and recovery boundaries

| State | Authority and persistence | Recovery consequence |
| --- | --- | --- |
| Node trust, desired state, operations and audit history | Controller database, accessed through domain Store contracts | Uncertain commits require keeping original operation identifiers and payloads. Client-side timeouts must not trigger uncoordinated duplicate submissions. |
| Command acceptance and results | Agent-owned SQLite journal | The local journal must survive process restarts and package upgrades. Purging it breaks replay detection and recovery auditing. |
| Privileged effects | Node-side effect records and signed privd receipts | Reconciliation demands cryptographic proofs tied to the specific command identifier. Reaching the expected configuration hash does not prove a historical invocation succeeded. |
| Certificate issuance and revocation | Signer's durable ledger and issuing identity | Signer ledgers and Controller datastores require coordinated restoration before resuming certificate operations. Persisted revocation entries do not automatically flush active client tunnels or publish certificate revocation lists (CRLs). |
| Health and session observations | Agent reports ingested into Controller read models | Telemetry freshness is distinct from transport reachability. A cached telemetry record shows historical observations and does not guarantee the daemon is currently responsive. |

Disruptions to the Controller or database halt workflows that require validation or durable writes. Unprocessed operations wait in the outbox until they expire or exhaust retries. If `Agent` or `privd` fails midway through execution, the state transition is indeterminate even if the network delivery succeeded. A Relay partition drops administrative transport. Recovery means restoring the Relay node, re-establishing secure sessions, and executing state reconciliation across connected nodes.

System recovery prioritizes the immutability of command logs, local journal entries, cryptographic identities, and monotonic sequence counters. Cold backups require an independently validated restoration workflow; they do not provide automatic failover or cross-version schema guarantees. Detailed specifications appear in [incident recovery](operations/incident-recovery.md), [Signer recovery](development/integrated-deployment-contract.md#compatibility-and-recovery), and [resilience coverage](development/resilience.md).

## Design tradeoffs and capacity

The Controller uses a modular monolithic architecture where domain services coordinate within shared database transactions. This guarantees atomic consistency between domain entity mutations and audit logging without distributed coordination overhead. Splitting these packages into microservices would require external distributed transaction management, which the system does not currently need. The [assembly code](../control-plane/internal/platform/app/app.go) (`Run`, then `runRoles`) selectively enables the API, worker and scheduler roles (`--role=api|worker|scheduler|all`) from one binary; production runs one `all` instance. The scheduler takes a database leader lease and ticks every 30 seconds. On SIGINT/SIGTERM or the first role failure, the lifecycle stops admission and SSE, shuts down the trust socket, then waits for tasks within `OCSERV_SHUTDOWN_TIMEOUT`.

The database-backed outbox pattern couples intent capture with pending delivery in a single transactional write, avoiding a separate message broker. As a result, task leasing, operational history, and primary business writes compete for the same database resources. Background polling loops ensure progress even if in-memory notification channels fail. While local `Agent` journaling mitigates redelivery issues, operations terminating in ambiguous states still require active reconciliation.

Splitting the processes across `transportd`, `Agent`, `privd`, and `upgrader` introduces IPC overhead and requires multi-process lifecycle coordination. In exchange, it isolates network dependencies and root privileges from control-plane domain logic. Merging these daemons into a single binary would collapse distinct privilege levels and security boundaries.

System throughput depends primarily on database row-level contention, worker polling latency, and serialized node-level task execution. The [worker](../control-plane/internal/operations/worker.go) processes claimed task batches in serial order, mirroring the sequential execution of mutating commands in the `Agent`. The global [command limit](../control-plane/internal/commandlimit/limit.go) prevents bufferbloat across nodes and workspaces; it is an admission control mechanism, not a guaranteed throughput rate.

Before modifying concurrency parameters or adding infrastructure, operators should profile system metrics—including queue depth, maximum uncommitted task age, transaction lock contention, and observation freshness—under representative workloads. [Queue metrics](development/command-reliability.md#controller-delivery) show dispatch latency, but an empty queue does not guarantee downstream execution succeeded. Formal service level objectives (SLOs) require empirical calibration against deployment profiles. Re-architecting subsystem boundaries should target reproducible performance bottlenecks and include fault-recovery evaluations.

## Safety model

- Production deployments use immutable, version-tagged or digest-pinned releases.
- Node packages are checked against an expected SHA-256 digest from the operator or an already-authorized Controller command before activation; a plain checksum detects corruption and is not an authorization key.
- Cryptographic material, including private keys, TLS certificates, relay tokens, and administrative credentials, is provisioned through isolated, out-of-band channels.
- High-risk mutating operations require approval from an independently authenticated principal bound to the exact request; self-approval is rejected. This does not attest that two different people control the principals ([approval principal boundary](reference/stable-contracts.md#approval-principal-boundary)).
- Audit records commit within the same database transaction as the changes they document.
- Formal disaster recovery procedures and verified rollback paths are prerequisites for production deployment.

## Development architecture

ocservia is a modular monorepo. The Controller operates as a domain-driven modular monolith, while transport mediation, privileged host execution, and package upgrades maintain isolated trust and lifecycle boundaries. The web tier decouples route composition, interaction workflows, API client adapters, and generated schema definitions. System evolution should extend these existing boundaries and avoid arbitrary abstractions or unnecessary package layers.

### Ownership and task navigation

Read the entries relevant to the change, not this entire list for every task.
This overview holds the current principles; the linked topics explain their
implementation and compatibility details.

| Boundary / task | Responsibility and dependency direction | Read next |
| --- | --- | --- |
| Startup and assembly | `cmd` and `platform/app` manage dependency injection, subsystem composition, and graceful process lifecycles. Domain packages must maintain inward dependency directions and never import assembly packages. | [`platform/app`](../control-plane/internal/platform/app/) |
| HTTP compatibility | Transport handlers deserialize request payloads, bind authorization contexts, and format responses. Business invariants and transactional state management remain inside domain services. | [HTTP baseline](development/http-baseline.md) |
| Controller services | Components interact via declared consumer interfaces matching concrete functional requirements. Avoid speculative abstractions or extra indirection layers. | [Consumer service boundaries](development/domain-service-boundaries.md) |
| Data and transactions | Domain store interfaces abstract relational persistence and driver dependencies. The initiating domain service governs the transaction boundary (`database.Tx`), allowing multiple storage adapters to enlist in the same atomic unit of work without domain code depending on concrete database drivers. | [`database` boundary](../control-plane/internal/database/database.go), [existing boundary check](../control-plane/internal/database/boundary_test.go), [operations and outbox](development/command-reliability.md#controller-delivery) |
| Runtime and privileges | `transportd` isolates control-plane overlay networking, running without host-level privileges. On managed hosts, `Agent`, `privd`, and `upgrader` execute under separated system identities and autonomous lifecycles. | [Transport](development/transportd.md), [Agent/privd](development/agent-privd.md), [Agent lifecycle](operations/agent-lifecycle.md) |
| Web API and Workspace | The frontend architecture separates route composition from interaction logic and client API adapters. Session coordination and workspace states must remain canonical without redundant state machines. | [Web API boundaries](development/web.md#api-boundaries), [node-detail features](development/web.md#node-detail-workflows) |
| Protocols and generated code | Protocol Buffers and OpenAPI definitions serve as normative schema contracts. IPC boundaries and cryptographic signatures enforce explicit, versioned serialization; raw protobuf byte streams must not be used as cryptographic signing inputs. Generated artifacts stay segregated from domain logic. | [Schema sources and generation](reference/stable-contracts.md#schema-sources-and-generation), [Agent/privd protocol](development/agent-privd.md), [command authorization](development/command-authorization-v1.md), [semantic hash v2](development/command-semantic-hash-v2.md) |
| Deployment | Operational deployments use versioned binaries, out-of-band cryptographic material, and existing lifecycle scripts. Do not introduce custom deployment frameworks. | [Production deployment](operations/production-deployment.md), [Controller upgrade](how-to/controller-lifecycle.md#upgrade), [Agent lifecycle](operations/agent-lifecycle.md) |
| Validation | Verification relies on targeted test coverage that validates behavioral invariants and contract guarantees. | [Validate a change](development/testing.md) |

### Source dependency view

Arrows point from importer to imported code. This is not the process graph in
[High-level view](#high-level-view).

```mermaid
flowchart LR
    subgraph Web["web/src"]
        Views["views"] --> Features["features, shared stores"]
        Features --> Adapters["api/*.ts adapters"]
        Adapters --> Generated["api/generated client"]
    end
    subgraph Go["control-plane"]
        Cmd["cmd/ocserv-control"] --> App["internal/platform/app"]
        App --> HTTP["internal/api"]
        App --> Domain["domain packages"]
        HTTP --> Domain
        Domain --> DB["internal/database interfaces"]
        App --> Drivers["database/postgres, mysql, connection"]
        Drivers --> DB
    end
    subgraph Rust["rust/crates"]
        Agent["agent"] --> Journal["command-journal"]
        Agent --> RustContracts["contracts, command-authorization"]
        Transportd["transportd"] --> RustContracts
        Privd["privd"] --> Adapter["ocserv-adapter"]
        Privd --> Upgrader["upgrader library"]
        Privd --> RustContracts
    end
```

Signer is a separate Go module (`signer/`, see `go.work`) and is reached only
over HTTPS. `privd` links the upgrader library to schedule the separate
`ocservia-upgrader@` unit; `agent` uses `transportd` only as a dev-dependency
for tests. Cargo manifests are the authority for crate dependencies.

The architectural diagrams show runtime interactions, which differ from source-level import hierarchies. Process composition, inter-process communication, and adapter binding are orthogonal concerns. The existing structure is normative; modifications require new architectural views only when clarifying novel trust or isolation boundaries.

### Intentional exceptions

- Defining an interface with a single implementation is valid when isolating capabilities across domain boundaries.
- Preserving atomic consistency may require transactions spanning multiple domain entities; transactional integrity takes precedence over rigid package separation.
- Request schema sanitization, concurrency locks during state re-verification, and daemon-level authorization checks safeguard distinct failure domains. They are non-redundant defense-in-depth measures.
- Stateful HTTP cookie handling in `auth`, storage-specific SQL semantics, and backward-compatible route mappings are exempt from uniform architectural layering when protocol constraints dictate.
- Elevated file sizes, shared data transfer objects (DTOs), and high package counts serve as diagnostic signals during code review; they are not inherently defects. Refactoring requires evidence of coupling or boundary violations.

### Evolution and decisions

Routine modifications within established boundaries only need standard change authorization. When adding capabilities that fit existing patterns, map them to the designated domain owner. Proposals modifying transaction boundaries, public schema contracts, privilege separations, or runtime architectures must document the requirements, technical constraints, trade-offs, alternative approaches, migration costs, and rollback strategies.

Subsystem decomposition into standalone services requires demonstrable needs for autonomous scalability, separate release lifecycles, or strict fault isolation. Intermediate caching layers or asynchronous message queues must address observed bottlenecks; speculative design does not justify them. Feasibility analyses are not commitments to adoption. Document any identified deviations transparently without unauthorized code refactoring or arbitrary relaxation of system invariants.

Architectural Decision Records (ADRs) document significant design trade-offs alongside subsystem documentation. Each record captures the status, context, decision, evaluated alternatives, consequences, verification protocols, and revisit triggers. Draft proposals are explicitly marked, and superseded records maintain links to subsequent decisions. Routine patches and localized enhancements do not warrant formal ADRs.

The [Integrated deployment contract](development/integrated-deployment-contract.md) documents decisions on single-host networking, the Signer subsystem, and reliable message delivery. The authoritative execution workflow is in the [deployment procedure](../deploy/production/integrated/README.md). The architectural contract does not confer runtime certification or alter the [production support policy](reference/support-policy.md).

Design documentation follows Lenciel's [How to write system design docs](https://lenciel.com/2021/01/how-to-write-system-design-docs/), grounding exposition in user execution paths, operational constraints, persistence models, and requirement-driven trade-offs. The methodology aligns with [Awesome Architecture](https://github.com/study8677/awesome-architecture) (chapters 02 and 08), structural diagrams observe the [C4 guidance](https://c4model.com/diagrams), and historical records use [Nygard's ADR method](https://www.cognitect.com/blog/2011/11/15/documenting-architecture-decisions). These frameworks are guidelines; concrete boundaries derive directly from the repository code and specifications.

## Where to read more

- [Deploy the Controller](getting-started/production.md)
- [Install a managed node](getting-started/managed-node.md)
- [Enroll a node](how-to/enroll-node.md)
- [Dedicated Relay](how-to/dedicated-relay.md)
- [Production deployment reference](operations/production-deployment.md)
- [Agent package lifecycle](operations/agent-lifecycle.md)
- [Technical reference](README.md#technical-reference)
