# Database major cutover acceptance

The previous checkpoint is the stable [v1.2.0 release](https://github.com/GentleKingson/ocservia/releases/tag/v1.2.0),
commit `169102557cd610847c9f6ac2083336cdcf82c483`. Both engines use epoch 1 /
revision 0. PostgreSQL's schema receipt is
`d837335f22c70858f4e2a5277332e478ecd6032e7b55f57d6512484bfab6f172`;
MySQL's is `3dc39a92ff4b54bff922872fae296843cea8dea1f2d1b36b42d86c9069dcb450`.
Moving main does not move this anchor. v1.2.0 remains an old-major checkpoint;
the epoch-2 cutover belongs to the next major.

## Active artifacts and supported paths

| Engine | Active SQL directory | Fresh result | Previous checkpoint transition |
| --- | --- | --- | --- |
| PostgreSQL 18.x | `control-plane/migrations` | Epoch 2 / revision 0 | Atomic verification, new baseline receipt, legacy cleanup and final validation |
| MySQL 8.4 LTS | `control-plane/internal/database/mysql/mysql` | Epoch 2 / revision 1 | Verified epoch 2 / revision 0, then eight journaled cleanup steps in revision 1 |

Each directory contains only `schema.sql` and `upgrade.sql` as SQL artifacts.
Each engine has only `schema_revisions` as its migration journal. The artifact
policy rejects extra SQL/JSON files and the old MySQL history directory.
Current-epoch receipts must match known raw schema/revision checksums; unsupported
epochs, revisions, foreign objects and mismatches are refused before mutation.
Older nonempty databases must first upgrade using v1.2.0. There is no history
adoption, reverse SQL or automatic dirty-state repair.

Fresh and upgraded databases have equivalent business schema, mandatory seeds
and runtime ACLs. Execution receipts describe the actual path: MySQL fresh
initialization executes 148 schema steps, while the checkpoint path records its
transition and eight cleanup steps. These are distinct valid histories, not
fabricated identical execution. MySQL repair resumes only the exact matching
artifact against verified before/after definitions. PostgreSQL failures roll
back the entire migration transaction. Both engines serialize migration and
discard a connection when lock release cannot be established.

## Removed history and retained acceptance

Deletion follows successful actual-checkpoint/fresh equivalence; historical
fixtures are loaded from the pinned Git release, outside the active SQL tree.

| Removed dependency | Replacement / retained coverage |
| --- | --- |
| 41 PostgreSQL numbered SQL files and snapshot descriptor | Fixed release fixture; independent current SQL author/freshness check; checkpoint/fresh schema, ACL and seed comparison |
| PostgreSQL legacy runner, snapshot-origin machinery and old journals | Atomic two-artifact executor; zero-mutation rejection, checksum tamper, cleanup rollback, concurrency and permissions |
| 30 MySQL numbered JSON files, baseline manifests, lineage archive and snapshot descriptors | Fixed release fixture; direct current SQL initialization and checkpoint/fresh schema, ACL and seed comparison |
| MySQL revision authoring/replay and old journals | Current revision executor; fresh and transition process kills, all eight cleanup DDL kill boundaries, exact repair, foreign replacement refusal, data rollback and drift rejection |
| Migration-21 audit preflight callback/interface | Runtime audit authenticity verification remains; checkpointed-tail transition, audit MAC/compaction and runtime mutation-denial behavior remain required |
| Legacy immutable-file hash ledger and filename-counter release metadata | Two-file artifact policy, raw artifact/history checksums and epoch/revision release metadata; old integer release manifests remain readable |

Authentication, scheduler, telemetry legacy-data operators, transaction cleanup,
row locking, TLS and permission tests remain business acceptance. Retiring old
schema-replay tests does not retire those behaviors. Required lifecycle entries
are listed explicitly in `scripts/required-go-tests.txt`; native execution must
have final passes and zero skips. Physical PostgreSQL and logical MySQL restore
must preserve the exact recorded receipts and support idempotent owner retry.

## Candidate qualification

PR07 provides PostgreSQL; PR08 adds MySQL; PR09 closes shared acceptance and
operator documentation. Qualify the final immutable stacked head with full
Basic CI (including both recovery jobs), Security Checks, Native Business
Diagnostics with production Signer and finite resilience, and build-only Release.
Record actual run URLs and head SHAs in the PR handoff; a dispatch without a
successful result is not evidence.

These open-branch results do not satisfy main-only Release Check. After separately
authorized merges, run Release Check and build-only validation on the exact final
main SHA. A major release requires its own approval; neither this document nor
the completed v1.2.0 checkpoint authorizes merging or publishing Phase B.
