package mysql

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/schemaartifact"
)

type schemaStatement struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Offset        int      `json:"offset"`
	Length        int      `json:"length"`
	Checksum      string   `json:"checksum"`
	SchemaHash    string   `json:"schema_hash"`
	RoundtripHash string   `json:"roundtrip_hash,omitempty"`
	Columns       []string `json:"columns,omitempty"`
}

type schemaSnapshot struct {
	Format           int               `json:"format"`
	Engine           Engine            `json:"engine"`
	Covered          int               `json:"covered_revision"`
	Parent           string            `json:"baseline_checksum"`
	RevisionChecksum string            `json:"revision_checksum"`
	HistoryChecksum  string            `json:"history_checksum"`
	SchemaChecksum   string            `json:"schema_checksum"`
	Statements       []schemaStatement `json:"statements"`
}

type snapshotArtifact struct {
	schemaSnapshot
	sum string
	sql []byte
}

const schemaArtifactHeader = "-- ocservia:artifact=schema\n-- ocservia:format=1\n-- ocservia:engine=mysql\n-- ocservia:epoch=1\n-- ocservia:revision=0\n\n"
const schemaStepEnd = "-- ocservia:end-step\n"

func schemaStepHeader(ordinal int, s schemaStatement) string {
	metadata, _ := json.Marshal(artifactStepMetadata{Kind: s.Kind, Object: s.Name, After: s.SchemaHash, RoundtripHash: s.RoundtripHash, Columns: s.Columns})
	return fmt.Sprintf("-- ocservia:step=%03d:%s_%s\n-- ocservia:metadata=%s\n", ordinal, s.Kind, s.Name, metadata)
}

func historyChecksum(chain []revisionArtifact, parent string, through int) (string, error) {
	if _, _, err := baselineFor(MySQL, parent); err != nil {
		return "", err
	}
	type entry struct {
		Version        int
		Name, Checksum string
	}
	entries := []entry{{1, "baseline", parent}}
	for _, r := range chain {
		if r.Version > through {
			break
		}
		if _, ok := r.Parents[parent]; !ok {
			return "", ErrChecksum
		}
		entries = append(entries, entry{r.Version, fmt.Sprintf("%06d.json", r.Version), r.sum})
	}
	if len(entries) != through {
		return "", ErrChecksum
	}
	data, _ := json.Marshal(entries)
	return digest(data), nil
}

func decodeSnapshot(data []byte, chain []revisionArtifact) (snapshotArtifact, error) {
	var a snapshotArtifact
	if json.Unmarshal(data, &a.schemaSnapshot) != nil || a.Format != 1 || a.Engine != MySQL || a.Covered < 30 || a.Covered > len(chain)+1 || len(a.Statements) < 6 {
		return a, ErrChecksum
	}
	history, err := historyChecksum(chain, a.Parent, a.Covered)
	if err != nil || history != a.HistoryChecksum || chain[a.Covered-2].sum != a.RevisionChecksum || len(a.SchemaChecksum) != 64 {
		return a, ErrChecksum
	}
	offset := 0
	marked := len(a.Statements) > 0 && a.Statements[0].Offset != 0
	if marked {
		offset = len(schemaArtifactHeader)
	}
	seen := map[string]bool{}
	for i, s := range a.Statements {
		if marked {
			offset += len(schemaStepHeader(i+1, s))
		}
		key := s.Kind + ":" + s.Name
		if !identifier.MatchString(s.Name) || seen[key] || s.Offset < 0 || s.Offset != offset || s.Length <= 0 || s.Length > int(^uint(0)>>1)-s.Offset-2 || len(s.Checksum) != 64 || len(s.SchemaHash) != 64 {
			return a, ErrChecksum
		}
		if s.Kind != "table" && s.Kind != "trigger" && s.Kind != "function" && s.Kind != "procedure" && s.Kind != "seed" {
			return a, ErrChecksum
		}
		for _, c := range s.Columns {
			if !identifier.MatchString(c) {
				return a, ErrChecksum
			}
		}
		if (s.Kind == "seed") != (len(s.Columns) > 0) || (s.RoundtripHash != "" && (s.Kind != "table" || len(s.RoundtripHash) != 64)) {
			return a, ErrChecksum
		}
		seen[key] = true
		offset = s.Offset + s.Length + 2
		if marked {
			offset += len(schemaStepEnd)
		}
	}
	if a.Statements[0].Name != "backend_schema_snapshot" || a.Statements[0].Kind != "table" || a.Statements[1].Name != "backend_schema_snapshot_steps" || a.Statements[1].Kind != "table" {
		return a, ErrChecksum
	}
	a.sum = digest(data)
	return a, nil
}

func currentSnapshot(chain []revisionArtifact) (snapshotArtifact, error) {
	data, err := manifests.ReadFile("mysql/schema.snapshot.json")
	if err != nil {
		return snapshotArtifact{}, ErrChecksum
	}
	a, err := decodeSnapshot(data, chain)
	if err != nil {
		return a, err
	}
	a.sql, err = manifests.ReadFile("mysql/schema.sql")
	if err != nil || digest(a.sql) != a.SchemaChecksum || a.Covered != chain[len(chain)-1].Version {
		return a, ErrChecksum
	}
	end := 0
	var parsed schemaartifact.Artifact
	marked := bytes.HasPrefix(a.sql, []byte("-- ocservia:"))
	if marked {
		parsed, err = schemaartifact.Parse(a.sql, "mysql")
		if err != nil || parsed.Kind != "schema" || parsed.Epoch != 1 || len(parsed.Baseline.Steps) != len(a.Statements) {
			return a, ErrChecksum
		}
	}
	for i, s := range a.Statements {
		if s.Offset < 0 || s.Offset > len(a.sql) || len(a.sql)-s.Offset < 2 || s.Length <= 0 || s.Length > len(a.sql)-s.Offset-2 {
			return a, ErrChecksum
		}
		end = s.Offset + s.Length
		if string(a.sql[end:end+2]) != ";\n" || digest(a.sql[s.Offset:end]) != s.Checksum {
			return a, ErrChecksum
		}
		if marked && !bytes.Equal(parsed.Baseline.Steps[i].SQL, a.sql[s.Offset:end+2]) {
			return a, ErrChecksum
		}
		if marked {
			metadata, _ := json.Marshal(artifactStepMetadata{Kind: s.Kind, Object: s.Name, After: s.SchemaHash, RoundtripHash: s.RoundtripHash, Columns: s.Columns})
			if !bytes.Equal(parsed.Baseline.Steps[i].Metadata, metadata) {
				return a, ErrChecksum
			}
		}
	}
	if !marked && end+2 != len(a.sql) {
		return a, ErrChecksum
	}
	return a, nil
}

// Older descriptors are immutable evidence of which statements really ran.
// Their SQL is unnecessary for forward upgrades; interrupted initialization
// still requires the old matching build, never a newer schema.sql.
func snapshotByChecksum(sum string, chain []revisionArtifact) (snapshotArtifact, error) {
	paths := []string{"mysql/schema.snapshot.json"}
	archived, _ := fs.Glob(manifests, "mysql/snapshots/*.json")
	paths = append(paths, archived...)
	for _, path := range paths {
		data, err := manifests.ReadFile(path)
		if err != nil || digest(data) != sum {
			continue
		}
		return decodeSnapshot(data, chain)
	}
	return snapshotArtifact{}, ErrChecksum
}

func snapshotSQL(a snapshotArtifact, s schemaStatement) string {
	return string(a.sql[s.Offset : s.Offset+s.Length])
}
