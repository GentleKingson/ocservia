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
Schema artifacts have revision zero and at least one step. Step ordinals run
from `001` through `999` without gaps; names are unique within a revision and
match `[a-z][a-z0-9_]{0,63}`.

Upgrade artifacts omit the revision header. Each revision uses
`-- ocservia:revision=N` and `-- ocservia:end-revision`, with at least one step.
Revisions start at one and are contiguous in file order. An upgrade artifact
may have no revisions when the baseline already represents the current schema.

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

One `-- ocservia:metadata={...}` line may immediately follow a step marker.
Its JSON object carries backend-specific verification metadata. The shared
parser validates the JSON boundary; the backend owns its fields, object
identities, fingerprints, and postcondition semantics. This permits SQL routine
bodies with internal semicolons while keeping executable SQL separate from
verification metadata.

Artifact and schema-baseline SHA-256 cover every file byte. Revision and
transition SHA-256 cover bytes after the opening boundary through the byte
before the closing boundary, including step markers and metadata. Step SHA-256
covers the raw SQL byte range after its marker/metadata through the byte before
`end-step`. No whitespace or line ending normalization occurs. Database
admission, journal validation, execution, locking, and repair remain the
responsibility of each backend.
