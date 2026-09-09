package mysql

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/semantictest"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

func TestRealSchemaTextRegex(t *testing.T) {
	b, _, _ := migrateFixture(t)
	ctx := context.Background()
	m, _, err := loadManifest(b.engine)
	if err != nil {
		t.Fatal(err)
	}
	patterns := map[string]string{}
	extract := regexp.MustCompile(`REGEXP '([^']*)'`)
	columns := regexp.MustCompile("(?m)^  `([a-z_]+)` (?:LONGTEXT|VARCHAR\\([0-9]+\\))")
	for _, s := range m.Steps {
		for _, v := range extract.FindAllStringSubmatch(s.SQL, -1) {
			actual := strings.ReplaceAll(v[1], `\\`, `\`)
			if !strings.HasPrefix(actual, "(?-i)") {
				t.Fatalf("%s: regex does not pin case sensitivity", s.Name)
			}
			pg := strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(actual, "(?-i)"), `\A`, "^"), `\z`, "$")
			patterns[pg] = actual
		}
		for _, c := range columns.FindAllStringSubmatch(s.SQL, -1) {
			if !strings.Contains(s.SQL, "LOCATE(0x00,CAST(`"+c[1]+"` AS BINARY))=0") {
				t.Fatalf("%s.%s lacks SQL-side NUL rejection", s.Name, c[1])
			}
		}
	}
	semantictest.PatternsMatch(t, patterns, func(value, pattern string) (bool, error) {
		var result bool
		err := b.QueryRow(ctx, `SELECT ? REGEXP ?`, value, pattern).Scan(&result)
		return result, err
	})
	id := UUIDBytes(uuid.New())
	now := time.Now().UTC()
	if _, err = b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,?,?,?,?)`, id, "name", "slug", now, now); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"a\x00b", `a\u0000b`, "a\ufffdb", "a ", "a  "} {
		_, err = b.Exec(ctx, `UPDATE workspaces SET name=? WHERE id=?`, value, id)
		if strings.ContainsRune(value, 0) {
			if !errors.Is(err, database.ErrConstraint) {
				t.Fatal("NUL not rejected by table CHECK", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var got string
		if err = b.QueryRow(ctx, `SELECT name FROM workspaces WHERE id=?`, id).Scan(&got); err != nil || got != value {
			t.Fatal("text changed", err)
		}
	}
	window, err := value.FromTime(now)
	if err != nil {
		t.Fatal(err)
	}
	expires, err := value.FromTime(now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, username := range []string{"alice", "Alice", "alice\n", "alice\r\n", "alice\u2028", "ali\u017fce"} {
		_, err = b.Exec(ctx, `INSERT INTO local_auth_attempts(username,window_until,expires_at) VALUES(?,?,?)`, username, window, expires)
		if username == "alice" {
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, database.ErrConstraint) {
			t.Fatalf("actual username CHECK accepted %q: %v", username, err)
		}
	}
	identity := UUIDBytes(uuid.New())
	if _, err = b.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES(?,?,?,?,?)`, identity, "issuer", "subject", window, window); err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{nil, "", "a\x00b"} {
		_, err = b.Exec(ctx, `UPDATE identities SET email=? WHERE id=?`, value, identity)
		if value == "a\x00b" {
			if !errors.Is(err, database.ErrConstraint) {
				t.Fatal("nullable NUL accepted", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRealExistingLongTextBounds(t *testing.T) {
	b, _, _ := migrateFixture(t)
	ctx := context.Background()
	workspace, node := UUIDBytes(uuid.New()), UUIDBytes(uuid.New())
	now := time.Now().UTC()
	if _, err := b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,?,?,?,?)`, workspace, "name", "slug", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,?,?,?,?)`, node, workspace, "node", "active", now, now); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"node_sessions", "user_usage_cursors", "secret_provider_refs"} {
		insert := func(value string) error {
			var err error
			switch table {
			case "node_sessions":
				_, err = b.Exec(ctx, `INSERT INTO node_sessions(node_id,session_id,username,client_ip,connected_at,bytes_in,bytes_out,observed_at) VALUES(?,?,?, ?,?,0,0,?)`, node, value, "alice", []byte{4, 32, 127, 0, 0, 1}, fixtureTimestamp(t, now), fixtureTimestamp(t, now))
			case "user_usage_cursors":
				_, err = b.Exec(ctx, `INSERT INTO user_usage_cursors(node_id,session_id,connected_at,username,rx_bytes,tx_bytes,observed_at) VALUES(?,?,?, ?,0,0,?)`, node, value, now, "alice", now)
			case "secret_provider_refs":
				_, err = b.Exec(ctx, `INSERT INTO secret_provider_refs(id,workspace_id,provider,key_path,version,state,created_at,updated_at) VALUES(?,?,?,?,'1','active',?,?)`, UUIDBytes(uuid.New()), workspace, "provider", value, now, now)
			}
			return err
		}
		semantictest.TextBounds(t, table, insert)
	}
}
