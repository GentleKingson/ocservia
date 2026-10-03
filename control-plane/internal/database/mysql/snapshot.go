package mysql

// Snapshot metadata is appended to every supported legacy lineage. Empty rows
// on those lineages do not assert that initialization used a snapshot.
const snapshotOriginDDL = `CREATE TABLE backend_schema_snapshot (
 singleton TINYINT PRIMARY KEY CHECK(singleton=1),
 artifact_checksum VARBINARY(64) NOT NULL,
 state VARBINARY(16) NOT NULL CHECK(state IN ('running','verified')),
 repair_count INT NOT NULL DEFAULT 0,
 started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 verified_at DATETIME(6) NULL
) ENGINE=InnoDB`

const snapshotStepsDDL = `CREATE TABLE backend_schema_snapshot_steps (
 ordinal INT PRIMARY KEY,
 name VARBINARY(64) NOT NULL UNIQUE,
 checksum VARBINARY(64) NOT NULL,
 state VARBINARY(16) NOT NULL CHECK(state IN ('running','verified')),
 started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 verified_at DATETIME(6) NULL
) ENGINE=InnoDB`
