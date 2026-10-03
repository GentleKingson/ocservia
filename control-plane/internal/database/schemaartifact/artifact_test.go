package schemaartifact

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
)

const schemaFixture = "-- ocservia:artifact=schema\n-- ocservia:format=1\n-- ocservia:engine=ENGINE\n-- ocservia:epoch=1\n-- ocservia:revision=0\n\n-- ocservia:step=001:journal\nCREATE TABLE schema_revisions(id INT);\n-- ocservia:end-step\n"

const revisionFixture = "-- ocservia:revision=1\n-- ocservia:step=001:trigger\n-- ocservia:metadata={\"kind\":\"trigger\",\"object\":\"example\"}\nCREATE TRIGGER example BEFORE INSERT ON sample FOR EACH ROW BEGIN SET NEW.id=1; SET NEW.value=2; END;\n-- ocservia:end-step\n-- ocservia:end-revision\n"

const upgradeHeader = "-- ocservia:artifact=upgrade\n-- ocservia:format=1\n-- ocservia:engine=ENGINE\n-- ocservia:epoch=1\n\n"

func TestRawSQLRangesAndChecksums(t *testing.T) {
	for _, engine := range []string{"postgresql", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			for _, fixture := range []string{schemaFixture, upgradeHeader + revisionFixture} {
				data := []byte(strings.ReplaceAll(fixture, "ENGINE", engine))
				a, err := Parse(data, engine)
				if err != nil {
					t.Fatal(err)
				}
				if a.Checksum != sha256.Sum256(data) {
					t.Fatal("artifact hash differs")
				}
				r := a.Baseline
				if a.Kind == "upgrade" {
					r = a.Revisions[0]
					body := strings.SplitN(revisionFixture, "\n", 2)[1]
					body = strings.TrimSuffix(body, "-- ocservia:end-revision\n")
					if r.Checksum != sha256.Sum256([]byte(body)) {
						t.Fatal("revision range differs")
					}
				}
				s := r.Steps[0]
				if s.Checksum != sha256.Sum256(s.SQL) || !bytes.HasSuffix(s.SQL, []byte(";\n")) {
					t.Fatal("SQL byte range differs")
				}
				before := bytes.Clone(s.SQL)
				for i := range data {
					data[i] = 'x'
				}
				if !bytes.Equal(s.SQL, before) {
					t.Fatal("input mutation changed parsed SQL")
				}
			}
		})
	}
}

func TestArtifactRejectsMalformedBoundaries(t *testing.T) {
	schema := strings.ReplaceAll(schemaFixture, "ENGINE", "mysql")
	upgrade := strings.ReplaceAll(upgradeHeader+revisionFixture, "ENGINE", "mysql")
	cases := map[string]string{
		"engine":                 strings.Replace(schema, "engine=mysql", "engine=postgresql", 1),
		"format":                 strings.Replace(schema, "format=1", "format=2", 1),
		"CRLF":                   strings.ReplaceAll(schema, "\n", "\r\n"),
		"missing LF":             strings.TrimSuffix(schema, "\n"),
		"missing epoch":          strings.Replace(schema, "-- ocservia:epoch=1\n", "", 1),
		"zero epoch":             strings.Replace(schema, "epoch=1", "epoch=0", 1),
		"duplicate header":       strings.Replace(schema, "epoch=1\n", "epoch=1\n-- ocservia:epoch=1\n", 1),
		"unknown marker":         strings.Replace(schema, "end-step", "endstep", 1),
		"indented marker":        strings.Replace(schema, "-- ocservia:end-step", " -- ocservia:end-step", 1),
		"misspelled prefix":      strings.Replace(schema, "-- ocservia:end-step", "--ocservia:end-step", 1),
		"outside SQL":            schema + "SELECT 1;\n",
		"empty step":             strings.Replace(schema, "CREATE TABLE schema_revisions(id INT);\n", "", 1),
		"unclosed step":          strings.Replace(schema, "-- ocservia:end-step\n", "", 1),
		"nested step":            strings.Replace(schema, "CREATE TABLE", "-- ocservia:step=002:nested\nCREATE TABLE", 1),
		"step gap":               strings.Replace(schema, "001:journal", "002:journal", 1),
		"step duplicate":         schema + "-- ocservia:step=002:journal\nSELECT 1;\n-- ocservia:end-step\n",
		"revision gap":           strings.Replace(upgrade, "revision=1", "revision=2", 1),
		"revision duplicate":     upgrade + revisionFixture,
		"unclosed revision":      strings.Replace(upgrade, "-- ocservia:end-revision\n", "", 1),
		"wrong closing boundary": strings.Replace(upgrade, "end-revision", "end-transition", 1),
		"malformed metadata":     strings.Replace(upgrade, `{"kind":"trigger","object":"example"}`, `{oops}`, 1),
		"array metadata":         strings.Replace(upgrade, `{"kind":"trigger","object":"example"}`, `[]`, 1),
		"late metadata":          strings.Replace(upgrade, "-- ocservia:end-step", "-- ocservia:metadata={}\n-- ocservia:end-step", 1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(data), "mysql"); err == nil {
				t.Fatal("accepted malformed artifact")
			}
		})
	}
}

func TestCheckpointTransitionAndEmptyUpgrade(t *testing.T) {
	header := strings.ReplaceAll(upgradeHeader, "ENGINE", "postgresql")
	if a, err := Parse([]byte(header), "postgresql"); err != nil || len(a.Revisions) != 0 {
		t.Fatal(a, err)
	}
	checkpoint := strings.Replace(header, "epoch=1\n", "epoch=2\n-- ocservia:previous-checkpoint-epoch=1\n-- ocservia:previous-checkpoint-revision=0\n-- ocservia:previous-checkpoint-ref=v1.2.0\n", 1)
	transition := "-- ocservia:transition=1:0->2:0\n-- ocservia:step=001:checkpoint\nSELECT 1;\n-- ocservia:end-step\n-- ocservia:end-transition\n"
	data := checkpoint + transition + revisionFixture
	a, err := Parse([]byte(data), "postgresql")
	if err != nil || a.Previous == nil || a.Transition == nil || len(a.Revisions) != 1 {
		t.Fatal(a, err)
	}
	for _, bad := range []string{checkpoint, strings.Replace(data, "1:0->2:0", "1:1->2:0", 1), checkpoint + revisionFixture + transition, strings.Replace(data, "checkpoint-epoch=1", "checkpoint-epoch=0", 1), strings.Replace(data, "-- ocservia:previous-checkpoint-ref=v1.2.0\n", "", 1)} {
		if _, err := Parse([]byte(bad), "postgresql"); err == nil {
			t.Fatal("accepted invalid transition")
		}
	}
}

func TestRevisionChecksumIncludesMetadataAndWhitespace(t *testing.T) {
	data := strings.ReplaceAll(upgradeHeader+revisionFixture, "ENGINE", "mysql")
	a, err := Parse([]byte(data), "mysql")
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range []string{strings.Replace(data, `"example"`, `"changed"`, 1), strings.Replace(data, "SET NEW.id=1", "SET  NEW.id=1", 1)} {
		b, err := Parse([]byte(changed), "mysql")
		if err != nil {
			t.Fatal(err)
		}
		if a.Revisions[0].Checksum == b.Revisions[0].Checksum {
			t.Fatal("changed revision bytes retained checksum")
		}
	}
}

func TestSchemaCheckpointReceipts(t *testing.T) {
	receipt := `{"checksum":"` + strings.Repeat("a", 64) + `","steps":1}`
	header := strings.ReplaceAll(upgradeHeader, "ENGINE", "mysql") + "-- ocservia:baseline=" + receipt + "\n"
	data := header + strings.Replace(revisionFixture, "-- ocservia:revision=1\n", "-- ocservia:revision=1\n-- ocservia:checkpoint="+receipt+"\n", 1)
	a, err := Parse([]byte(data), "mysql")
	if err != nil || a.Base == nil || a.Revisions[0].Checkpoint == nil {
		t.Fatal(a, err)
	}
	schema := strings.ReplaceAll(schemaFixture, "ENGINE", "mysql")
	schema = strings.Replace(schema, "revision=0", "revision=1", 1)
	if a, err := Parse([]byte(schema), "mysql"); err != nil || a.Baseline.Number != 1 {
		t.Fatal(a, err)
	}
	for _, bad := range []string{
		strings.Replace(data, receipt, `{"checksum":"bad","steps":1}`, 1),
		strings.Replace(data, receipt, strings.Replace(receipt, `"steps":1`, `"steps":0`, 1), 1),
		strings.Replace(data, receipt, strings.Replace(receipt, `"steps":1`, `"steps":1,"steps":1`, 1), 1),
		strings.Replace(data, receipt, strings.Replace(receipt, `"steps":1`, `"steps":1,"unknown":true`, 1), 1),
		strings.Replace(data, "-- ocservia:end-step", "-- ocservia:checkpoint="+receipt+"\n-- ocservia:end-step", 1),
	} {
		if _, err := Parse([]byte(bad), "mysql"); err == nil {
			t.Fatal("invalid checkpoint receipt accepted")
		}
	}
}
