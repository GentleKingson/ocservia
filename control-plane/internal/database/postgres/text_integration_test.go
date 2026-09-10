package postgres

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/semantictest"
	"github.com/google/uuid"
)

func TestSchemaTextRegexIntegration(t *testing.T) {
	b := testBackend(t)
	ctx := context.Background()
	rows, err := b.Query(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE contype='c' AND connamespace='public'::regnamespace`)
	if err != nil {
		t.Fatal(err)
	}
	patterns := map[string]string{}
	extract := regexp.MustCompile(`!?~ '([^']*)'::text`)
	for rows.Next() {
		var definition string
		if err = rows.Scan(&definition); err != nil {
			t.Fatal(err)
		}
		for _, m := range extract.FindAllStringSubmatch(definition, -1) {
			patterns[m[1]] = m[1]
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	semantictest.PatternsMatch(t, patterns, func(value, pattern string) (bool, error) {
		var result bool
		err := b.QueryRow(ctx, `SELECT $1::text ~ $2::text`, value, pattern).Scan(&result)
		return result, err
	})
	// Actual PostgreSQL text input rejects NUL, but accepts a literal backslash
	// sequence and the replacement character. SQL NULL remains distinct.
	for _, value := range []string{"a\x00b", `a\u0000b`, "a\ufffdb"} {
		var roundtrip string
		err := b.QueryRow(ctx, `SELECT $1::text`, value).Scan(&roundtrip)
		if strings.ContainsRune(value, 0) {
			if err == nil {
				t.Fatal("NUL accepted")
			}
		} else if err != nil || roundtrip != value {
			t.Fatal("text changed", err)
		}
	}
}

func TestExistingLongTextBoundsIntegration(t *testing.T) {
	b := testBackend(t)
	ctx := context.Background()
	// Copy real column definitions, CHECKs and unique indexes, not retyped
	// expectations. LIKE does not copy FKs; fixtures stay connection-local.
	for _, table := range []string{"node_sessions", "user_usage_cursors", "secret_provider_refs"} {
		if _, err := b.Exec(ctx, "CREATE TEMP TABLE probe_"+table+" (LIKE public."+table+" INCLUDING CONSTRAINTS INCLUDING INDEXES)"); err != nil {
			t.Fatal(err)
		}
		node, workspace := uuid.New(), uuid.New()
		now := time.Now().UTC()
		insert := func(value string) error {
			var err error
			switch table {
			case "node_sessions":
				_, err = b.Exec(ctx, `INSERT INTO probe_node_sessions(node_id,session_id,username,client_ip,connected_at,bytes_in,bytes_out,observed_at) VALUES($1,$2,'alice','127.0.0.1',$3,0,0,$3)`, node, value, now)
			case "user_usage_cursors":
				_, err = b.Exec(ctx, `INSERT INTO probe_user_usage_cursors(node_id,session_id,connected_at,username,rx_bytes,tx_bytes,observed_at) VALUES($1,$2,$3,'alice',0,0,$3)`, node, value, now)
			case "secret_provider_refs":
				_, err = b.Exec(ctx, `INSERT INTO probe_secret_provider_refs(id,workspace_id,provider,key_path,version,state,created_at,updated_at) VALUES($1,$2,'provider',$3,'1','active',$4,$4)`, uuid.New(), workspace, value, now)
			}
			return err
		}
		semantictest.TextBounds(t, table, insert)
	}
}
