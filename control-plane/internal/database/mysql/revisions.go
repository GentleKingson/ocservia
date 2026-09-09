package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// A revision appends to either published version-1 lineage. Baseline receipts
// remain byte-for-byte intact; they never become receipts for a different SQL
// artifact. The version-2 receipt names its actual parent checksum explicitly.
type revisionStep struct {
	Name           string `json:"name"`
	Object         string `json:"object,omitempty"`
	Kind           string `json:"kind,omitempty"`
	SQL            string `json:"sql"`
	Checksum       string `json:"checksum"`
	Before         string `json:"before"`
	After          string `json:"after"`
	VerifySQL      string `json:"verify_sql,omitempty"`
	CheckBeforeSQL string `json:"check_before_sql,omitempty"`
	Repairable     bool   `json:"repairable,omitempty"`
}
type revisionPlan struct {
	Steps []revisionStep `json:"steps"`
}
type revision struct {
	Version                 int                     `json:"version"`
	PreviousChecksum        string                  `json:"previous_checksum,omitempty"`
	Engine                  Engine                  `json:"engine"`
	ControllerSchema        int                     `json:"controller_schema"`
	MinimumControllerSchema int                     `json:"minimum_controller_schema"`
	Parents                 map[string]revisionPlan `json:"parents"`
	MetadataHashes          map[string]string       `json:"metadata_hashes"`
}

const revisionsDDL = `CREATE TABLE IF NOT EXISTS backend_schema_revisions (
 version INT PRIMARY KEY CHECK(version>=2),
 parent_checksum VARBINARY(64) NOT NULL,
 manifest_checksum VARBINARY(64) NOT NULL,
 state VARBINARY(16) NOT NULL CHECK(state IN ('running','verified')),
 repair_count INT NOT NULL DEFAULT 0,
 started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 verified_at DATETIME(6) NULL
) ENGINE=InnoDB`

const revisionStepsDDL = `CREATE TABLE IF NOT EXISTS backend_schema_revision_steps (
 version INT NOT NULL, ordinal INT NOT NULL,
 name VARBINARY(64) NOT NULL, checksum VARBINARY(64) NOT NULL,
 state VARBINARY(16) NOT NULL CHECK(state IN ('running','verified')),
 started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 verified_at DATETIME(6) NULL,
 PRIMARY KEY(version,ordinal), UNIQUE KEY(version,name),
 FOREIGN KEY(version) REFERENCES backend_schema_revisions(version) ON DELETE RESTRICT
) ENGINE=InnoDB`

func baselineFor(engine Engine, checksum string) (manifest, string, error) {
	current, sum, err := loadManifest(engine)
	if err != nil {
		return current, "", err
	}
	if checksum == "" || checksum == sum {
		return current, sum, nil
	}
	data, err := manifests.ReadFile("history/f6cd0e0/" + string(engine) + ".json")
	if err != nil {
		return manifest{}, "", ErrChecksum
	}
	old, oldSum, err := decodeManifest(engine, data)
	if err != nil || checksum != oldSum {
		return manifest{}, "", ErrChecksum
	}
	return old, oldSum, nil
}

func loadRevision(engine Engine) (revision, string, error) {
	var r revision
	data, err := manifests.ReadFile(string(engine) + "/000002.json")
	if err != nil || json.Unmarshal(data, &r) != nil {
		return r, "", ErrChecksum
	}
	if r.Engine != engine || r.Version != 2 || r.ControllerSchema != 34 || r.MinimumControllerSchema != 34 || len(r.Parents) != 2 {
		return r, "", ErrChecksum
	}
	if len(r.MetadataHashes) != 2 || len(r.MetadataHashes["backend_schema_revisions"]) != 64 || len(r.MetadataHashes["backend_schema_revision_steps"]) != 64 {
		return r, "", ErrChecksum
	}
	for parent, p := range r.Parents {
		if len(parent) != 64 {
			return r, "", ErrChecksum
		}
		baseline, _, err := baselineFor(engine, parent)
		if err != nil {
			return r, "", err
		}
		before := map[string]string{}
		for _, s := range baseline.Steps {
			if s.Kind == "table" {
				before[s.Name] = s.SchemaHash
			}
		}
		seen := map[string]bool{}
		for _, s := range p.Steps {
			if seen[s.Name] || !identifier.MatchString(s.Name) || before[s.Name] != s.Before || len(s.After) != 64 || s.Before == s.After || digest([]byte(s.SQL)) != s.Checksum {
				return r, "", ErrChecksum
			}
			seen[s.Name] = true
		}
	}
	return r, digest(data), nil
}

func (b *Backend) rootOn(ctx context.Context, conn *sql.Conn) (manifest, string, error) {
	var exists int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='backend_migrations'`).Scan(&exists); err != nil {
		return manifest{}, "", safeError(err)
	}
	var sum string
	if exists != 0 {
		err := conn.QueryRowContext(ctx, `SELECT manifest_checksum FROM backend_migrations WHERE singleton=1`).Scan(&sum)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return manifest{}, "", safeError(err)
		}
	}
	return baselineFor(b.engine, sum)
}

// revisionTables validates any already-created metadata before using it. Two
// independent DDL commits may be interrupted; neither implies a business step
// ran or a revision completed.
func revisionTables(ctx context.Context, conn *sql.Conn, r revision) (int, error) {
	n := 0
	for _, name := range []string{"backend_schema_revisions", "backend_schema_revision_steps"} {
		actual, err := schemaHash(ctx, conn, step{Name: name, Kind: "table"})
		if err != nil {
			return 0, err
		}
		if actual == "" {
			continue
		}
		if len(r.MetadataHashes[name]) != 64 || actual != r.MetadataHashes[name] {
			return 0, ErrSchema
		}
		n++
	}
	return n, nil
}
