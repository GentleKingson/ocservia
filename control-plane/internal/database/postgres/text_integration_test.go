package postgres

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var schemaRegexPatterns = map[string]string{
	`^[a-z0-9][a-z0-9._-]*$`:                     "alice",
	`^[a-z][a-z0-9_]{0,63}$`:                     "reason_code",
	`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`: "00-" + strings.Repeat("a", 32) + "-" + strings.Repeat("b", 16) + "-01",
	`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`:          "Alice",
	`^[A-Za-z0-9._-]+$`:                          "key-A",
	`^[A-Za-z0-9_.-]{1,128}$`:                    "key-A",
	`^ed25519-sha256:[0-9a-f]{64}$`:              "ed25519-sha256:" + strings.Repeat("a", 64),
	`(^|/)\.\.(/|$)`:                             "../secret",
	`^[A-Za-z0-9._-]{1,64}$`:                     "provider-A",
	`^password-change:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`: "password-change:01234567-89ab-7def-8123-456789abcdef",
}

func TestSchemaTextRegexIntegration(t *testing.T) {
	b := testBackend(t)
	ctx := context.Background()
	rows, err := b.Query(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE contype='c' AND connamespace='public'::regnamespace`)
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]string{}
	extract := regexp.MustCompile(`!?~ '([^']*)'::text`)
	for rows.Next() {
		var definition string
		if err = rows.Scan(&definition); err != nil {
			t.Fatal(err)
		}
		for _, m := range extract.FindAllStringSubmatch(definition, -1) {
			actual[m[1]] = m[1]
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(actual) != len(schemaRegexPatterns) {
		t.Fatalf("schema regex inventory changed: got %d want %d", len(actual), len(schemaRegexPatterns))
	}
	for pg, seed := range schemaRegexPatterns {
		pattern, ok := actual[pg]
		if !ok {
			t.Fatalf("missing schema regex %q", pg)
		}
		t.Run(pg, func(t *testing.T) {
			oracle := regexp.MustCompile(pg)
			values := []string{"", seed, strings.Repeat("a", 64), strings.Repeat("a", 65), strings.Repeat("a", 128), strings.Repeat("a", 129), "a/../b", "a/..", "a/..\n", "a/..\r\n", "a/..\u2028", "a/.../b", "a/./b", "a/..x", "..x"}
			for r := rune(1); r < 128; r++ {
				values = append(values, string(r), seed+string(r), string(r)+seed)
			}
			for _, s := range []string{"é", "İ", "ſ", "K", "Ａ", "١", "\u0085", "\u2028", "\u2029", "\r\n", " ", "\n\n"} {
				values = append(values, s, seed+s, s+seed)
			}
			for _, v := range values {
				var got bool
				if err := b.QueryRow(ctx, `SELECT $1::text ~ $2::text`, v, pattern).Scan(&got); err != nil {
					t.Fatalf("value %q: %v", v, err)
				}
				if want := oracle.MatchString(v); got != want {
					t.Fatalf("value %q: got %v want %v", v, got, want)
				}
			}
		})
	}
	for _, v := range []string{"a\x00b", `a\u0000b`, "a\ufffdb"} {
		var roundtrip string
		err := b.QueryRow(ctx, `SELECT $1::text`, v).Scan(&roundtrip)
		if strings.ContainsRune(v, 0) {
			if err == nil {
				t.Fatal("NUL accepted")
			}
		} else if err != nil || roundtrip != v {
			t.Fatal("text changed", err)
		}
	}
}

func TestExistingLongTextBoundsIntegration(t *testing.T) {
	b := testBackend(t)
	ctx := context.Background()
	for _, table := range []string{"node_sessions", "user_usage_cursors", "secret_provider_refs"} {
		if _, err := b.Exec(ctx, "CREATE TEMP TABLE probe_"+table+" (LIKE public."+table+" INCLUDING CONSTRAINTS INCLUDING INDEXES)"); err != nil {
			t.Fatal(err)
		}
		node, workspace := uuid.New(), uuid.New()
		now := time.Now().UTC()
		insert := func(v string) error {
			var err error
			switch table {
			case "node_sessions":
				_, err = b.Exec(ctx, `INSERT INTO probe_node_sessions(node_id,session_id,username,client_ip,connected_at,bytes_in,bytes_out,observed_at) VALUES($1,$2,'alice','127.0.0.1',$3,0,0,$3)`, node, v, now)
			case "user_usage_cursors":
				_, err = b.Exec(ctx, `INSERT INTO probe_user_usage_cursors(node_id,session_id,connected_at,username,rx_bytes,tx_bytes,observed_at) VALUES($1,$2,$3,'alice',0,0,$3)`, node, v, now)
			case "secret_provider_refs":
				_, err = b.Exec(ctx, `INSERT INTO probe_secret_provider_refs(id,workspace_id,provider,key_path,version,state,created_at,updated_at) VALUES($1,$2,'provider',$3,'1','active',$4,$4)`, uuid.New(), workspace, v, now)
			}
			return err
		}
		t.Run(table, func(t *testing.T) {
			maximum := 256
			if table == "secret_provider_refs" {
				maximum = 512
				for _, v := range []string{"a/..\n", "a/..\r\n", "a/..\u2028", "a/.../b"} {
					if err := insert(v); err != nil {
						t.Fatalf("non-traversal path %q rejected: %v", v, err)
					}
				}
				for _, v := range []string{"../a", "a/..", "a/../b"} {
					if err := insert(v); err == nil {
						t.Fatalf("traversal path %q accepted", v)
					}
				}
			}
			for _, unit := range []string{"a", "\U0001f642"} {
				for _, n := range []int{191, 192, maximum - 1, maximum} {
					v := strings.Repeat(unit, n)
					if err := insert(v); err != nil {
						t.Fatalf("%d characters rejected: %v", n, err)
					}
					if err := insert(v); err == nil {
						t.Fatal("exact duplicate accepted")
					}
				}
				if err := insert(strings.Repeat(unit, maximum-1) + " "); err != nil {
					t.Fatal("trailing space changed uniqueness", err)
				}
				for _, v := range []string{"x" + strings.Repeat(unit, maximum), "y" + strings.Repeat(unit, maximum-1) + " ", "z" + strings.Repeat(unit, maximum-1) + "  "} {
					if err := insert(v); err == nil {
						t.Fatal("existing length CHECK bypassed or trailing spaces silently truncated")
					}
				}
			}
		})
	}
}
