# SQL artifact format 1

The parser recognizes whole-line `-- ocservia:` markers at column zero. Files
use LF endings and end with LF. It does not split on SQL semicolons or interpret
SQL syntax. SQL outside a step, malformed/reserved markers, duplicate headers,
unknown fields, and unmatched boundaries are errors.

Every artifact starts with these headers (each exactly once):

```sql
-- ocservia:artifact=schema
-- ocservia:format=1
-- ocservia:engine=postgresql
-- ocservia:epoch=1
-- ocservia:revision=0

-- ocservia:step=001:journal
CREATE TABLE schema_revisions (...);
-- ocservia:end-step
```

Engines are `postgresql` and `mysql`. Epochs are positive decimal integers.
Schema artifacts declare their current checkpoint revision and have at least
one step. Revision zero is the epoch baseline. Step ordinals run
from `001` through `999` without gaps; names are unique within a revision and
match `[a-z][a-z0-9_]{0,63}`.

Upgrade artifacts omit the revision header. Each revision uses
`-- ocservia:revision=N` and `-- ocservia:end-revision`, with at least one step.
Revisions start at one and are contiguous in file order. An upgrade artifact
may have no revisions when the baseline already represents the current schema.

Executors require a `-- ocservia:baseline={...}` upgrade header containing the
original epoch-baseline schema checksum and step count. Backend verification
metadata can be included in that receipt. For example:

```text
{"checksum":"<64 lowercase hex characters>","steps":1,"metadata":{"catalog_sha256":"<64 lowercase hex characters>"}}
```

A revision may immediately start with `-- ocservia:checkpoint={...}` using the
same receipt format. This pins a fresh schema snapshot at that revision. JSON
receipt keys use the canonical order `checksum`, `steps`, optional `metadata`;
unknown or duplicate keys, noncanonical encoding, and invalid hashes/counts
are rejected. A fresh install stamps one schema checkpoint at its declared
revision, without inventing executed rows for the revisions it covers. Later
revisions are appended with their SQL block checksums. The two receipt forms
are accepted only at their explicitly pinned journal positions.

For a schema checkpoint above revision zero, a `history-sha256` header binds
the covered SQL revisions. Its input is the engine, epoch and baseline checksum
separated by tabs and terminated by LF, followed by one `revision<TAB>checksum<LF>`
record per covered revision. This detects mutations even when a fresh database
has never executed those SQL revisions.

A previous-epoch transition requires all three checkpoint headers:

```sql
-- ocservia:previous-checkpoint-epoch=1
-- ocservia:previous-checkpoint-revision=0
-- ocservia:previous-checkpoint-ref=v1.2.0
```

The previous epoch must be the current epoch minus one. Before ordinary
revisions, exactly one `-- ocservia:transition=1:0->2:0` block must match the
declared checkpoint and end with `-- ocservia:end-transition`. The reference
must be fixed by the release process; parsing it does not establish trust in
that release or a database.

Executors additionally require `previous-checkpoint-receipt` to pin the exact
journal checksum, step count, and necessary backend schema verification metadata
at that checkpoint. A syntactically valid release reference is insufficient.

One `-- ocservia:metadata={...}` line may immediately follow a step marker.
Its JSON object carries backend-specific verification metadata. The shared
parser validates the JSON boundary; the backend owns its fields, object
identities, fingerprints, and postcondition semantics. This permits SQL routine
bodies with internal semicolons while keeping executable SQL separate from
verification metadata.

Artifact and schema-baseline SHA-256 cover every file byte. Revision and
transition SHA-256 cover bytes after the opening boundary through the byte
before the closing boundary, including step markers and metadata. A revision's
optional checkpoint receipt is excluded from that SQL block hash to avoid a
circular dependency with its covered schema checksum; it remains covered by
the full artifact checksum. A previous-epoch transition keeps its raw SQL
block checksum and step count in the new epoch revision-zero row; its retained
previous receipt distinguishes it from a fresh schema checkpoint. Step SHA-256
covers the raw SQL byte range after its marker/metadata through the byte before
`end-step`. No whitespace or line ending normalization occurs. Database
admission, journal validation, execution, locking, and repair remain the
responsibility of each backend.
