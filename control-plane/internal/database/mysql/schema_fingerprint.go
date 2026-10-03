package mysql

import (
	"encoding/json"
	"io/fs"
	"sync"
)

var schemaFingerprintOnce sync.Once
var historicalSchemaFingerprints map[string]bool
var roundtripFingerprints map[string]string

// MySQL 8.4 SHOW CREATE roundtrips a range literal from >= -211813488000000000
// to >= -(211813488000000000). Both denote the same signed integer. Preserve
// exact historical hashes first, and accept the presentation alternative only
// when it matches an immutable catalog fingerprint; no constraint is removed.
func tableFingerprint(definition string) string {
	exact := digest([]byte(definition))
	schemaFingerprintOnce.Do(func() {
		historicalSchemaFingerprints = map[string]bool{}
		roundtripFingerprints = map[string]string{}
		paths, _ := fs.Glob(manifests, "mysql/snapshots/*.json")
		paths = append(paths, "mysql/schema.snapshot.json")
		for _, path := range paths {
			data, err := manifests.ReadFile(path)
			if err != nil {
				continue
			}
			var a schemaSnapshot
			if json.Unmarshal(data, &a) != nil {
				continue
			}
			for _, s := range a.Statements {
				if s.Kind == "table" && s.RoundtripHash != "" {
					roundtripFingerprints[s.RoundtripHash] = s.SchemaHash
				}
			}
		}
		chain, err := loadRevisionChain(MySQL)
		if err != nil {
			return
		}
		for parent := range chain[0].Parents {
			root, _, err := baselineFor(MySQL, parent)
			if err != nil {
				continue
			}
			for _, s := range root.Steps {
				historicalSchemaFingerprints[s.SchemaHash] = true
			}
			for _, hash := range root.MetadataHashes {
				historicalSchemaFingerprints[hash] = true
			}
		}
		for _, r := range chain {
			for _, plan := range r.Parents {
				for _, s := range plan.Steps {
					historicalSchemaFingerprints[s.Before] = true
					historicalSchemaFingerprints[s.After] = true
				}
			}
			for _, hash := range r.MetadataHashes {
				historicalSchemaFingerprints[hash] = true
			}
		}
	})
	if historicalSchemaFingerprints[exact] {
		return exact
	}
	if original, ok := roundtripFingerprints[exact]; ok && historicalSchemaFingerprints[original] {
		return original
	}
	return exact
}
